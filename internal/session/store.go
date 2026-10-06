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

const (
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
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('chat', ?)`, channel+":"+chatID); err != nil {
		db.Close()
		return nil, err
	}
	return &Session{Channel: channel, ChatID: chatID, dir: dir, db: db}, nil
}

func (s *Session) close() error { return s.db.Close() }

// Append stores messages atomically, in order.
func (s *Session) Append(ctx context.Context, msgs ...llm.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, m := range msgs {
		var calls any
		if len(m.ToolCalls) > 0 {
			b, err := json.Marshal(m.ToolCalls)
			if err != nil {
				return err
			}
			calls = string(b)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages(role, content, tool_calls, tool_call_id, tool_name, created_at) VALUES (?,?,?,?,?,?)`,
			string(m.Role), m.Content, calls, m.ToolCallID, m.Name, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Messages returns the sanitised message history, oldest first.
func (s *Session) Messages(ctx context.Context) ([]Stored, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, content, tool_calls, tool_call_id, tool_name FROM messages ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Stored
	for rows.Next() {
		var st Stored
		var role string
		var calls sql.NullString
		if err := rows.Scan(&st.ID, &role, &st.Message.Content, &calls, &st.Message.ToolCallID, &st.Message.Name); err != nil {
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
	Role string // "user" or "assistant"
	Text string
}

// Recent returns up to n of the latest conversational messages, oldest
// first, leaving out tool calls and tool results.
func (s *Session) Recent(ctx context.Context, n int) ([]Turn, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content FROM messages
		 WHERE role = 'user' OR (role = 'assistant' AND content != '')
		 ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []Turn
	for rows.Next() {
		var t Turn
		if err := rows.Scan(&t.Role, &t.Text); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
func (s *Session) Reset(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE key IN (?, ?)`, metaSummary, metaLastPromptTokens); err != nil {
		return err
	}
	return tx.Commit()
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
