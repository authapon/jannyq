// Package session persists per-chat conversation state. Each chat (a user or
// a group on a channel) has its own SQLite database file.
package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/authapon/jannyq/internal/llm"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	role         TEXT NOT NULL,
	content      TEXT NOT NULL DEFAULT '',
	tool_calls   TEXT,
	tool_call_id TEXT NOT NULL DEFAULT '',
	tool_name    TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);
`

// schemaVersion is the current value of PRAGMA user_version.
const schemaVersion = 2

// migrate brings a database created by an older version up to date.
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if v < 1 { // who said it, and when
		for _, stmt := range []string{
			`ALTER TABLE messages ADD COLUMN sender_id   TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE messages ADD COLUMN sender_name TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE messages ADD COLUMN sent_at     TEXT NOT NULL DEFAULT ''`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
	}
	if v < 2 { // files sent with messages
		for _, stmt := range []string{
			`CREATE TABLE IF NOT EXISTS attachments (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				message_id   INTEGER NOT NULL,
				kind         TEXT NOT NULL,
				name         TEXT NOT NULL,
				mime         TEXT NOT NULL DEFAULT '',
				size         INTEGER NOT NULL DEFAULT 0,
				path         TEXT NOT NULL DEFAULT '',
				text_path    TEXT NOT NULL DEFAULT '',
				pages        INTEGER NOT NULL DEFAULT 0,
				chars        INTEGER NOT NULL DEFAULT 0,
				inline       INTEGER NOT NULL DEFAULT 0,
				images       TEXT NOT NULL DEFAULT '',
				note         TEXT NOT NULL DEFAULT '',
				stored_bytes INTEGER NOT NULL DEFAULT 0,
				created_at   INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS attachments_message ON attachments(message_id)`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

const (
	metaGroup            = "group"
	metaSummary          = "summary"
	metaLastPromptTokens = "last_prompt_tokens"
)

// Session is the persisted state of one chat. Use it only inside
// Manager.With, which serialises access per chat.
type Session struct {
	Channel string
	ChatID  string
	dir     string
	db      *sql.DB
}

// Dir returns the directory that holds this chat's files.
func (s *Session) Dir() string { return s.dir }

func openSession(channel, chatID, dir string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "session.db")
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init session db: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate session db: %w", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('chat', ?)`, channel+":"+chatID); err != nil {
		db.Close()
		return nil, err
	}
	return &Session{Channel: channel, ChatID: chatID, dir: dir, db: db}, nil
}

func (s *Session) close() error { return s.db.Close() }

// Append stores messages atomically, in order.
func (s *Session) Append(ctx context.Context, msgs ...llm.Message) error {
	entries := make([]Entry, len(msgs))
	for i, m := range msgs {
		entries[i] = Entry{Message: m}
	}
	return s.AppendEntries(ctx, entries...)
}

// AppendEntries stores messages, with their sender and send time, atomically
// and in order. It is safe to call while a request for this chat is running.
func (s *Session) AppendEntries(ctx context.Context, entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, e := range entries {
		m := e.Message
		var calls any
		if len(m.ToolCalls) > 0 {
			b, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return err
			}
			calls = string(b)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages(role, content, tool_calls, tool_call_id, tool_name, created_at, sender_id, sender_name, sent_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			string(m.Role), m.Content, calls, m.ToolCallID, m.Name, now, e.SenderID, e.SenderName, e.SentAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AppendEntry stores one message and returns its id. Like AppendEntries it is
// safe to call while a request for this chat is running.
func (s *Session) AppendEntry(ctx context.Context, e Entry) (int64, error) {
	m := e.Message
	var calls any
	if len(m.ToolCalls) > 0 {
		b, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return 0, err
		}
		calls = string(b)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(role, content, tool_calls, tool_call_id, tool_name, created_at, sender_id, sender_name, sent_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		string(m.Role), m.Content, calls, m.ToolCallID, m.Name, time.Now().Unix(), e.SenderID, e.SenderName, e.SentAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// LastID returns the id of the newest stored message (0 when there is none).
func (s *Session) LastID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM messages`).Scan(&id)
	return id.Int64, err
}

// Messages returns the sanitised message history, oldest first.
func (s *Session) Messages(ctx context.Context) ([]Stored, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, content, tool_calls, tool_call_id, tool_name, sender_id, sender_name, sent_at, created_at
		 FROM messages ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Stored
	for rows.Next() {
		var st Stored
		var role string
		var calls sql.NullString
		if err := rows.Scan(&st.ID, &role, &st.Message.Content, &calls, &st.Message.ToolCallID, &st.Message.Name,
			&st.SenderID, &st.SenderName, &st.SentAt, &st.CreatedAt); err != nil {
			return nil, err
		}
		st.Message.Role = llm.Role(role)
		if calls.Valid && calls.String != "" {
			if err := json.Unmarshal([]byte(calls.String), &st.Message.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode tool calls of message %d: %w", st.ID, err)
			}
		}
		all = append(all, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sanitizeHistory(all), nil
}

// Turn is one user message or assistant reply, as shown to people.
type Turn struct {
	Role        string // "user" or "assistant"
	Text        string
	Attachments []string // names of the files sent with a user message
}

// Recent returns up to n of the latest conversational messages, oldest
// first, leaving out tool calls and tool results.
func (s *Session) Recent(ctx context.Context, n int) ([]Turn, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, content FROM messages
		 WHERE role = 'user' OR (role = 'assistant' AND content != '')
		 ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	var turns []Turn
	var ids []int64
	for rows.Next() {
		var t Turn
		var id int64
		if err := rows.Scan(&id, &t.Role, &t.Text); err != nil {
			rows.Close()
			return nil, err
		}
		turns = append(turns, t)
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		names, err := s.AttachmentsFrom(ctx, ids[len(ids)-1])
		if err != nil {
			return nil, err
		}
		for i, id := range ids {
			for _, a := range names[id] {
				turns[i].Attachments = append(turns[i].Attachments, a.Name)
			}
		}
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	return turns, nil
}

// Count returns the number of conversational messages (user messages and
// non-empty assistant replies), ignoring tool plumbing.
func (s *Session) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE role = 'user' OR (role = 'assistant' AND content != '')`).Scan(&n)
	return n, err
}

// Meta returns a metadata value, or "" when unset.
func (s *Session) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta stores a metadata value.
func (s *Session) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// metaIntroduced marks a chat in which the bot has introduced itself. A reset
// keeps it: a person who clears the conversation has met the bot already.
const metaIntroduced = "introduced"

// FirstReply reports whether this is the first time the bot answers in the
// chat, and notes that it has. It is true once per chat, even when several
// messages arrive together. A chat that already holds answers (one that
// existed before introductions did) is marked without being introduced to.
func (s *Session) FirstReply(ctx context.Context) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES (?, '1')`, metaIntroduced)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// greeted already, unless the greeting failed and is waiting for a retry
		res, err = s.db.ExecContext(ctx, `UPDATE meta SET value = '1' WHERE key = ? AND value = 'retry'`, metaIntroduced)
		if err != nil {
			return false, err
		}
		n, _ = res.RowsAffected()
		return n > 0, nil
	}
	var answered bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages WHERE role = 'assistant' AND content != '')`).Scan(&answered)
	return !answered, err
}

// RetryFirstReply makes FirstReply true once more, for when the greeting could
// not be made and should be tried again with the next message.
func (s *Session) RetryFirstReply(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE meta SET value = 'retry' WHERE key = ?`, metaIntroduced)
	return err
}

// IsGroup reports whether the chat is a group (several people talk to the bot).
func (s *Session) IsGroup(ctx context.Context) bool {
	v, _ := s.Meta(ctx, metaGroup)
	return v == "1"
}

// SetGroup records whether the chat is a group.
func (s *Session) SetGroup(ctx context.Context, group bool) error {
	v := "0"
	if group {
		v = "1"
	}
	return s.SetMeta(ctx, metaGroup, v)
}

// Prune deletes the oldest messages so that about keep conversational
// messages remain, and returns how many rows were removed. It is the last
// resort for chats that keep growing although compaction cannot run.
func (s *Session) Prune(ctx context.Context, keep int) (int64, error) {
	if keep < 1 {
		keep = 1
	}
	var cutoff int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM messages WHERE role = 'user' OR (role = 'assistant' AND content != '')
		 ORDER BY id DESC LIMIT 1 OFFSET ?`, keep-1).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	if err := s.deleteAttachments(ctx, `message_id < ?`, cutoff); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Summary returns the running summary of compacted messages ("" if none).
func (s *Session) Summary(ctx context.Context) (string, error) { return s.Meta(ctx, metaSummary) }

// LastPromptTokens returns the prompt size of the most recent model call.
func (s *Session) LastPromptTokens(ctx context.Context) int {
	v, _ := s.Meta(ctx, metaLastPromptTokens)
	var n int
	fmt.Sscanf(v, "%d", &n)
	return n
}

// SetLastPromptTokens records the prompt size of the latest model call.
func (s *Session) SetLastPromptTokens(ctx context.Context, n int) error {
	return s.SetMeta(ctx, metaLastPromptTokens, fmt.Sprint(n))
}

// Compact replaces all messages with ID <= upToID by summary.
func (s *Session) Compact(ctx context.Context, upToID int64, summary string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id <= ?`, upToID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaSummary, summary); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, metaLastPromptTokens); err != nil {
		return err
	}
	return tx.Commit()
}

// Reset forgets the whole conversation.
func (s *Session) Reset(ctx context.Context) error { return s.ResetUpTo(ctx, math.MaxInt64) }

// ResetUpTo forgets the conversation as far as message id upTo and the summary
// of it. Messages stored later are kept: they were sent after the reset was
// asked for, even if the reset itself had to wait its turn.
func (s *Session) ResetUpTo(ctx context.Context, upTo int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id <= ?`, upTo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?)`, metaSummary, metaLastPromptTokens); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// forgetting the conversation includes the files that were sent in it
	return s.deleteAttachments(ctx, `message_id <= ?`, upTo)
}

// sessionDir maps a chat to its directory: <base>/<channel>/<name>. The name
// combines a sanitised prefix (for humans) with a hash of the raw ID (for
// uniqueness), so arbitrary platform IDs can never escape the base directory.
func sessionDir(base, channel, chatID string) string {
	sum := sha256.Sum256([]byte(chatID))
	name := sanitize(chatID, 24) + "-" + hex.EncodeToString(sum[:8])
	return filepath.Join(base, sanitize(channel, 32), name)
}

func sanitize(s string, max int) string {
	var sb strings.Builder
	for _, r := range s {
		if sb.Len() >= max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "_"
	}
	return sb.String()
}
