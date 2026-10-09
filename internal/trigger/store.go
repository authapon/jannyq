package trigger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Modes: what happens when a trigger comes due.
const (
	ModeRemind = "remind" // the model reminds the user, in its own words, without tools
	ModeTask   = "task"   // the model carries out the instruction, with its tools, and reports
)

// Statuses.
const (
	StatusActive   = "active"
	StatusPaused   = "paused"   // switched off by the user
	StatusRunning  = "running"  // a one-time trigger that has been taken up
	StatusDone     = "done"     // a one-time trigger that has fired
	StatusDisabled = "disabled" // switched off by the bot: the owner may no longer use it, ...
)

// Trigger is a scheduled reminder or task.
type Trigger struct {
	ID        int64
	Channel   string
	ChatID    string
	IsGroup   bool
	OwnerID   string
	OwnerName string
	Mode      string
	Title     string // a short name for lists
	Text      string // the reminder, or the instruction of the task
	Cron      string // the cron expression of a recurring trigger; "" for a one-time one
	At        time.Time
	Zone      string // IANA name of the time zone the schedule is read in
	Next      time.Time
	Status    string

	Created    time.Time
	LastRun    time.Time
	LastResult string // "ok", "failed", "missed", "skipped"
	LastError  string
	Runs       int
	Fails      int
}

// Recurring reports whether the trigger repeats.
func (t Trigger) Recurring() bool { return t.Cron != "" }

// Location returns the trigger's time zone (UTC when the name is unknown).
func (t Trigger) Location() *time.Location {
	if loc, err := time.LoadLocation(t.Zone); err == nil && t.Zone != "" {
		return loc
	}
	return time.UTC
}

const schema = `
CREATE TABLE IF NOT EXISTS triggers (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	channel     TEXT NOT NULL,
	chat_id     TEXT NOT NULL,
	is_group    INTEGER NOT NULL DEFAULT 0,
	owner_id    TEXT NOT NULL DEFAULT '',
	owner_name  TEXT NOT NULL DEFAULT '',
	mode        TEXT NOT NULL,
	title       TEXT NOT NULL DEFAULT '',
	text        TEXT NOT NULL,
	cron        TEXT NOT NULL DEFAULT '',
	at_unix     INTEGER NOT NULL DEFAULT 0,
	zone        TEXT NOT NULL DEFAULT '',
	next_unix   INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL DEFAULT 'active',
	created     INTEGER NOT NULL,
	last_run    INTEGER NOT NULL DEFAULT 0,
	last_result TEXT NOT NULL DEFAULT '',
	last_error  TEXT NOT NULL DEFAULT '',
	runs        INTEGER NOT NULL DEFAULT 0,
	fails       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS triggers_chat ON triggers(channel, chat_id);
CREATE INDEX IF NOT EXISTS triggers_due ON triggers(status, next_unix);
`

// Store keeps triggers in a SQLite file.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating it if needed) the database at path. A one-time trigger
// that was taken up when the process stopped is made due again.
func Open(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init trigger db: %w", err)
	}
	if _, err := db.Exec(`UPDATE triggers SET status = 'active' WHERE status = 'running'`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

const columns = `id, channel, chat_id, is_group, owner_id, owner_name, mode, title, text, cron, at_unix, zone, next_unix,
	status, created, last_run, last_result, last_error, runs, fails`

func scan(row interface{ Scan(...any) error }) (Trigger, error) {
	var t Trigger
	var group int
	var at, next, created, last int64
	err := row.Scan(&t.ID, &t.Channel, &t.ChatID, &group, &t.OwnerID, &t.OwnerName, &t.Mode, &t.Title, &t.Text, &t.Cron, &at, &t.Zone,
		&next, &t.Status, &created, &last, &t.LastResult, &t.LastError, &t.Runs, &t.Fails)
	t.IsGroup = group != 0
	t.At, t.Next, t.Created, t.LastRun = fromUnix(at), fromUnix(next), fromUnix(created), fromUnix(last)
	return t, err
}

// ErrNotFound is returned for an id that does not exist (in that chat).
var ErrNotFound = errors.New("trigger: not found")

// Create stores a new trigger and returns its id.
func (s *Store) Create(ctx context.Context, t Trigger) (int64, error) {
	if t.Status == "" {
		t.Status = StatusActive
	}
	if t.Created.IsZero() {
		t.Created = s.now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO triggers(channel, chat_id, is_group, owner_id, owner_name, mode, title, text, cron,
		at_unix, zone, next_unix, status, created) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Channel, t.ChatID, boolInt(t.IsGroup), t.OwnerID, t.OwnerName, t.Mode, t.Title, t.Text, t.Cron,
		unix(t.At), t.Zone, unix(t.Next), t.Status, unix(t.Created))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Get returns a trigger of a chat.
func (s *Store) Get(ctx context.Context, channel, chatID string, id int64) (Trigger, error) {
	t, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM triggers WHERE id = ? AND channel = ? AND chat_id = ?`, id, channel, chatID))
	if errors.Is(err, sql.ErrNoRows) {
		return Trigger{}, ErrNotFound
	}
	return t, err
}

// List returns the triggers of a chat, oldest first.
func (s *Store) List(ctx context.Context, channel, chatID string) ([]Trigger, error) {
	return s.query(ctx, `SELECT `+columns+` FROM triggers WHERE channel = ? AND chat_id = ? ORDER BY id`, channel, chatID)
}

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		t, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Count returns how many triggers a chat has that are not finished.
func (s *Store) Count(ctx context.Context, channel, chatID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM triggers WHERE channel = ? AND chat_id = ? AND status != 'done'`, channel, chatID).Scan(&n)
	return n, err
}

// Update stores the changeable fields of a trigger of a chat.
func (s *Store) Update(ctx context.Context, t Trigger) error {
	res, err := s.db.ExecContext(ctx, `UPDATE triggers SET mode = ?, title = ?, text = ?, cron = ?, at_unix = ?, zone = ?, next_unix = ?, status = ?
		WHERE id = ? AND channel = ? AND chat_id = ?`,
		t.Mode, t.Title, t.Text, t.Cron, unix(t.At), t.Zone, unix(t.Next), t.Status, t.ID, t.Channel, t.ChatID)
	return affected(res, err)
}

// Delete removes a trigger of a chat.
func (s *Store) Delete(ctx context.Context, channel, chatID string, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM triggers WHERE id = ? AND channel = ? AND chat_id = ?`, id, channel, chatID)
	return affected(res, err)
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Due returns the active triggers whose time has come, earliest first.
func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]Trigger, error) {
	return s.query(ctx, `SELECT `+columns+` FROM triggers WHERE status = 'active' AND next_unix > 0 AND next_unix <= ? ORDER BY next_unix LIMIT ?`,
		now.Unix(), limit)
}

// NextDue returns the time of the earliest trigger that is still to come.
func (s *Store) NextDue(ctx context.Context) (time.Time, bool, error) {
	var n sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_unix) FROM triggers WHERE status = 'active' AND next_unix > 0`).Scan(&n); err != nil {
		return time.Time{}, false, err
	}
	if !n.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(n.Int64, 0), true, nil
}

// Claim takes a due trigger up, once: it moves a recurring trigger on to its
// next time (newNext) or marks a one-time one as running. It reports false when
// somebody else changed the trigger since it was read.
func (s *Store) Claim(ctx context.Context, t Trigger, newNext time.Time) (bool, error) {
	status := StatusActive
	if !t.Recurring() {
		status = StatusRunning
	} else if newNext.IsZero() {
		status = StatusDone // a schedule that never comes again
	}
	next := unix(newNext)
	if !t.Recurring() {
		next = unix(t.Next)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE triggers SET next_unix = ?, status = ? WHERE id = ? AND status = 'active' AND next_unix = ?`,
		next, status, t.ID, unix(t.Next))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Finish records a run. A one-time trigger becomes done, whatever the result.
func (s *Store) Finish(ctx context.Context, t Trigger, result, errText string) error {
	status := ""
	if !t.Recurring() {
		status = StatusDone
	}
	failed := 0
	if result == "failed" {
		failed = 1
	}
	_, err := s.db.ExecContext(ctx, `UPDATE triggers SET last_run = ?, last_result = ?, last_error = ?, runs = runs + 1, fails = fails + ?,
		status = CASE WHEN ? != '' THEN ? ELSE status END, next_unix = CASE WHEN ? = 'done' THEN 0 ELSE next_unix END WHERE id = ?`,
		s.now().Unix(), result, errText, failed, status, status, status, t.ID)
	return err
}

// Note records what happened to an occurrence that was not run (a missed one),
// without counting it as a run.
func (s *Store) Note(ctx context.Context, id int64, result, errText string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE triggers SET last_run = ?, last_result = ?, last_error = ? WHERE id = ?`, s.now().Unix(), result, errText, id)
	return err
}

// Disable switches a trigger off for a reason the user should be able to read.
func (s *Store) Disable(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE triggers SET status = 'disabled', next_unix = 0, last_result = 'disabled', last_error = ? WHERE id = ?`, reason, id)
	return err
}

// Prune deletes finished one-time triggers that were last run before cutoff.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM triggers WHERE status = 'done' AND last_run > 0 AND last_run < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOrphans deletes the triggers of chats for which exists says no (the
// chat was deleted, e.g. by the retention sweep) and returns how many.
func (s *Store) DeleteOrphans(ctx context.Context, exists func(channel, chatID string) bool) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT channel, chat_id FROM triggers`)
	if err != nil {
		return 0, err
	}
	var gone [][2]string
	for rows.Next() {
		var ch, chat string
		if err := rows.Scan(&ch, &chat); err != nil {
			rows.Close()
			return 0, err
		}
		if !exists(ch, chat) {
			gone = append(gone, [2]string{ch, chat})
		}
	}
	rows.Close()
	n := 0
	for _, g := range gone {
		res, err := s.db.ExecContext(ctx, `DELETE FROM triggers WHERE channel = ? AND chat_id = ?`, g[0], g[1])
		if err != nil {
			return n, err
		}
		k, _ := res.RowsAffected()
		n += int(k)
	}
	return n, nil
}

// Stats counts active triggers by mode (for the metrics).
func (s *Store) Stats(ctx context.Context) (remind, task int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT mode, COUNT(*) FROM triggers WHERE status = 'active' GROUP BY mode`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var mode string
		var n int
		if err := rows.Scan(&mode, &n); err != nil {
			return 0, 0, err
		}
		if mode == ModeTask {
			task = n
		} else {
			remind += n
		}
	}
	return remind, task, rows.Err()
}
