package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Attachment is a file a user sent with a message, as it was processed. The
// files themselves live under <session dir>/files; the paths here are
// relative to the session directory.
type Attachment struct {
	ID        int64
	MessageID int64
	Kind      string // "image", "pdf" or "text"
	Name      string // the name the user's file had
	MIME      string
	Size      int64 // bytes of the original
	// Path is the processed file: the resized image, or the original of a document.
	Path string
	// TextPath holds the extracted text of a document ("" if none).
	TextPath string
	Pages    int
	Chars    int
	// Inline is true when the text is shown to the model with the message.
	Inline bool
	// Images are the pictures shown to the model for this attachment: the
	// photo itself, or the rendered pages of a scanned document.
	Images []string
	Note   string
	// StoredBytes is the disk space used by all of this attachment's files.
	StoredBytes int64
}

// filesDir is where a chat's attachment files are kept.
const filesDir = "files"

// FilesDir returns the directory for the chat's attachment files, creating it.
func (s *Session) FilesDir() (string, error) {
	d := filepath.Join(s.dir, filesDir)
	return d, os.MkdirAll(d, 0o750)
}

// FilePath resolves a path stored in an Attachment to a file on disk. Paths
// that point outside the session directory are refused.
func (s *Session) FilePath(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", errors.New("session: invalid file path")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("session: invalid file path")
	}
	return filepath.Join(s.dir, clean), nil
}

// AddAttachment stores an attachment of message messageID and returns its id.
func (s *Session) AddAttachment(ctx context.Context, a Attachment) (int64, error) {
	images, _ := json.Marshal(a.Images)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO attachments(message_id, kind, name, mime, size, path, text_path, pages, chars, inline, images, note, stored_bytes, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.MessageID, a.Kind, a.Name, a.MIME, a.Size, a.Path, a.TextPath, a.Pages, a.Chars, boolInt(a.Inline),
		string(images), a.Note, a.StoredBytes, time.Now().Unix())
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

const attachmentColumns = `id, message_id, kind, name, mime, size, path, text_path, pages, chars, inline, images, note, stored_bytes`

func scanAttachment(row interface{ Scan(...any) error }) (Attachment, error) {
	var a Attachment
	var inline int
	var images string
	err := row.Scan(&a.ID, &a.MessageID, &a.Kind, &a.Name, &a.MIME, &a.Size, &a.Path, &a.TextPath,
		&a.Pages, &a.Chars, &inline, &images, &a.Note, &a.StoredBytes)
	if err != nil {
		return a, err
	}
	a.Inline = inline != 0
	if images != "" {
		_ = json.Unmarshal([]byte(images), &a.Images)
	}
	return a, nil
}

// AttachmentByID returns one attachment, or sql.ErrNoRows.
func (s *Session) AttachmentByID(ctx context.Context, id int64) (Attachment, error) {
	return scanAttachment(s.db.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE id = ?`, id))
}

// AttachmentsFrom returns the attachments of messages with id >= fromMessageID,
// grouped by message.
func (s *Session) AttachmentsFrom(ctx context.Context, fromMessageID int64) (map[int64][]Attachment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments WHERE message_id >= ? ORDER BY id`, fromMessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out[a.MessageID] = append(out[a.MessageID], a)
	}
	return out, rows.Err()
}

// removeFiles deletes attachment files, ignoring paths that are invalid or
// already gone.
func (s *Session) removeFiles(paths []string) {
	for _, rel := range paths {
		if rel == "" {
			continue
		}
		if p, err := s.FilePath(rel); err == nil {
			_ = os.Remove(p)
		}
	}
}

// attachmentPaths lists every file of an attachment.
func attachmentPaths(a Attachment) []string {
	paths := append([]string{a.Path, a.TextPath}, a.Images...)
	return paths
}

// deleteAttachments removes the attachments matching a condition, rows and
// files. where is a trusted SQL fragment.
func (s *Session) deleteAttachments(ctx context.Context, where string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE `+where, args...)
	if err != nil {
		return err
	}
	var doomed []Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			rows.Close()
			return err
		}
		doomed = append(doomed, a)
	}
	rows.Close()
	if len(doomed) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE `+where, args...); err != nil {
		return err
	}
	for _, a := range doomed {
		s.removeFiles(attachmentPaths(a))
	}
	return nil
}

// EvictAttachments deletes the oldest attachments until the files of the
// chat use at most maxBytes. Attachments of messages that were compacted away
// go first; those of messages still in the conversation only if nothing else
// helps. It returns the number of attachments removed.
func (s *Session) EvictAttachments(ctx context.Context, maxBytes int64) (int, error) {
	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT SUM(stored_bytes) FROM attachments`).Scan(&total); err != nil {
		return 0, err
	}
	if total.Int64 <= maxBytes {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+attachmentColumns+`,
		       EXISTS (SELECT 1 FROM messages m WHERE m.id = attachments.message_id) AS alive
		FROM attachments ORDER BY alive ASC, id ASC`)
	if err != nil {
		return 0, err
	}
	var candidates []Attachment
	for rows.Next() {
		var a Attachment
		var inline, alive int
		var images string
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Kind, &a.Name, &a.MIME, &a.Size, &a.Path, &a.TextPath,
			&a.Pages, &a.Chars, &inline, &images, &a.Note, &a.StoredBytes, &alive); err != nil {
			rows.Close()
			return 0, err
		}
		if images != "" {
			_ = json.Unmarshal([]byte(images), &a.Images)
		}
		candidates = append(candidates, a)
	}
	rows.Close()
	removed := 0
	for _, a := range candidates {
		if total.Int64 <= maxBytes {
			break
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id = ?`, a.ID); err != nil {
			return removed, err
		}
		s.removeFiles(attachmentPaths(a))
		total.Int64 -= a.StoredBytes
		removed++
	}
	return removed, nil
}

// ListAttachments returns up to limit attachments, newest first.
func (s *Session) ListAttachments(ctx context.Context, limit int) ([]Attachment, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+attachmentColumns+` FROM attachments ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WriteFile stores data as a new file in the chat's files directory and returns
// its path relative to the session directory. The name is generated; ext
// (with its dot) is only a hint for people looking at the directory.
func (s *Session) WriteFile(data []byte, ext string) (string, error) {
	dir, err := s.FilesDir()
	if err != nil {
		return "", err
	}
	if len(ext) > 8 || strings.ContainsAny(ext, `/\`) {
		ext = ""
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	name := fmt.Sprintf("%d-%s%s", time.Now().UnixNano(), hex.EncodeToString(rnd[:]), ext)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o640); err != nil {
		return "", err
	}
	return filesDir + "/" + name, nil
}
