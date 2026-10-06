package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/authapon/jannyq/internal/attach"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/ratelimit"
	"github.com/authapon/jannyq/internal/sandbox"
	"github.com/authapon/jannyq/internal/session"
)

// AttachConfig turns on file attachments.
type AttachConfig struct {
	Processor *attach.Processor
	// MaxPerMessage is how many files of one message are read (default 5).
	MaxPerMessage int
	// RatePerMinute limits files per user per minute (default 10).
	RatePerMinute int
	// MaxChatBytes caps the disk space of one chat's files; the oldest are
	// deleted beyond it (default 200 MiB).
	MaxChatBytes int64
	// Inbox, when set, copies each file into the chat's command workspace.
	Inbox func(ctx context.Context, workspace, path string, data []byte) error
}

type attachState struct {
	cfg     AttachConfig
	limiter *ratelimit.Limiter
}

// SetAttachments enables reading files that users send.
func (r *Router) SetAttachments(c AttachConfig) {
	if c.Processor == nil {
		r.attach = nil
		return
	}
	if c.MaxPerMessage <= 0 {
		c.MaxPerMessage = 5
	}
	if c.RatePerMinute <= 0 {
		c.RatePerMinute = 10
	}
	if c.MaxChatBytes <= 0 {
		c.MaxChatBytes = 200 << 20
	}
	r.attach = &attachState{cfg: c, limiter: ratelimit.New(c.RatePerMinute, time.Minute)}
}

// note is what the model is told when a file could not be read.
func attachNote(err error) string {
	switch {
	case errors.Is(err, attach.ErrCorrupt):
		return "the file looks damaged"
	case errors.Is(err, attach.ErrTooLarge), errors.Is(err, attach.ErrImageTooBig):
		return "the file is too large"
	case errors.Is(err, attach.ErrUnsupported):
		return "unsupported type of file"
	case errors.Is(err, attach.ErrEncrypted):
		return "the PDF is password protected"
	case errors.Is(err, attach.ErrTooManyPages):
		return "the document has too many pages"
	case errors.Is(err, attach.ErrVisionOff):
		return "you cannot look at pictures"
	case errors.Is(err, attach.ErrScanned):
		return "a scanned PDF and no text recognition is available"
	}
	return "an error occurred while reading it"
}

// userReason is the same reason in the user's language.
func (r *Router) userReason(err error) string {
	switch {
	case errors.Is(err, attach.ErrCorrupt):
		return r.tr.T("attach_err_corrupt")
	case errors.Is(err, attach.ErrTooLarge):
		return r.tr.T("attach_err_too_large", humanBytes(r.attach.cfg.Processor.Config().MaxBytes))
	case errors.Is(err, attach.ErrImageTooBig):
		return r.tr.T("attach_err_image_too_big")
	case errors.Is(err, attach.ErrUnsupported):
		return r.tr.T("attach_err_unsupported")
	case errors.Is(err, attach.ErrEncrypted):
		return r.tr.T("attach_err_encrypted")
	case errors.Is(err, attach.ErrTooManyPages):
		return r.tr.T("attach_err_pages", r.attach.cfg.Processor.Config().PDFMaxPages)
	case errors.Is(err, attach.ErrVisionOff):
		return r.tr.T("attach_err_vision")
	case errors.Is(err, attach.ErrScanned):
		return r.tr.T("attach_err_scanned")
	case errors.Is(err, errDownload):
		return r.tr.T("attach_err_download")
	}
	return r.tr.T("attach_err_generic")
}

var errDownload = errors.New("download failed")

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// markUnread records that a file was sent but not read, so the conversation
// still shows that it existed and why it is unavailable.
func (r *Router) markUnread(ctx context.Context, s *session.Session, msgID int64, name, note string) {
	if _, err := s.AddAttachment(ctx, session.Attachment{MessageID: msgID, Kind: attach.KindUnread, Name: name, Note: note}); err != nil {
		r.log.Warn("could not note an unread attachment", "err", err)
	}
}

func displayName(a channel.Attachment, i int) string {
	n := strings.TrimSpace(a.Name)
	if n == "" {
		n = fmt.Sprintf("file-%d", i+1)
	}
	return n
}

// recordUnread notes the files of a group message that was not addressed to the
// bot. They are not downloaded: reading every file of a busy group would cost
// far more than anyone benefits from.
func (r *Router) recordUnread(ctx context.Context, s *session.Session, msgID int64, atts []channel.Attachment) {
	for i, a := range atts {
		if i >= r.attach.cfg.MaxPerMessage {
			break
		}
		r.markUnread(ctx, s, msgID, attach.CleanName(displayName(a, i)), "not addressed to the assistant, so it was not opened")
	}
}

// ingest downloads and reads the files of a stored message, attaches the
// results to it and reports what could not be read to the user. It returns
// how many files were read. It runs inside the chat's session lock.
func (r *Router) ingest(ctx context.Context, s *session.Session, in channel.Incoming, msgID int64) int {
	st := r.attach
	workspace := sandbox.WorkspaceID(in.Channel + ":" + in.ChatID)
	limiterKey := in.Channel + ":" + in.UserID
	if in.UserID == "" || in.Origin != "" {
		limiterKey = in.Channel + ":" + in.Origin
	}
	var problems []string
	read := 0
	tooMany, limited := false, false
	for i, a := range in.Attachments {
		name := attach.CleanName(displayName(a, i))
		fail := func(err error) {
			r.markUnread(ctx, s, msgID, name, attachNote(err))
			problems = append(problems, r.tr.T("attach_cannot_read", name, r.userReason(err)))
		}
		switch {
		case i >= st.cfg.MaxPerMessage:
			r.markUnread(ctx, s, msgID, name, "too many files in one message")
			tooMany = true
			continue
		case !st.limiter.Allow(limiterKey):
			r.markUnread(ctx, s, msgID, name, "the user is sending files too quickly")
			limited = true
			continue
		}
		if err := r.readOne(ctx, s, in, msgID, workspace, a, name); err != nil {
			if ctx.Err() != nil {
				return read
			}
			r.log.Info("attachment not read", "channel", in.Channel, "chat", in.ChatID, "name", name, "err", err)
			fail(err)
			continue
		}
		read++
	}
	if tooMany {
		problems = append(problems, r.tr.T("attach_too_many", st.cfg.MaxPerMessage))
	}
	if limited {
		problems = append(problems, r.tr.T("attach_rate_limited"))
	}
	if len(problems) > 0 {
		r.say(ctx, in, strings.Join(problems, "\n"))
	}
	if n, err := s.EvictAttachments(ctx, st.cfg.MaxChatBytes); err != nil {
		r.log.Warn("could not free attachment space", "err", err)
	} else if n > 0 {
		r.log.Info("old attachments deleted to free space", "channel", in.Channel, "chat", in.ChatID, "count", n)
	}
	return read
}

func (r *Router) readOne(ctx context.Context, s *session.Session, in channel.Incoming, msgID int64, workspace string, a channel.Attachment, name string) error {
	proc := r.attach.cfg.Processor
	max := proc.Config().MaxBytes
	if a.Size > max {
		return attach.ErrTooLarge
	}
	if a.Fetch == nil {
		return errDownload
	}
	data, err := a.Fetch(ctx, max)
	switch {
	case errors.Is(err, attach.ErrTooLarge), errors.Is(err, channel.ErrTooLarge):
		return attach.ErrTooLarge
	case err != nil:
		r.log.Warn("could not download an attachment", "channel", in.Channel, "err", err)
		return errDownload
	}
	res, err := proc.Process(ctx, workspace, attach.Input{Name: name, MIME: a.MIME, Data: data})
	if err != nil {
		return err
	}

	var written []string
	cleanup := func() {
		for _, rel := range written {
			if p, err := s.FilePath(rel); err == nil {
				_ = removeFile(p)
			}
		}
	}
	write := func(b []byte, ext string) (string, error) {
		rel, err := s.WriteFile(b, ext)
		if err == nil {
			written = append(written, rel)
		}
		return rel, err
	}
	rec := session.Attachment{MessageID: msgID, Kind: res.Kind, Name: res.Name, MIME: res.MIME,
		Size: int64(len(data)), Pages: res.Pages, Chars: res.Chars, Inline: res.Inline, Note: res.Note}
	var inboxData []byte
	if res.Kind == attach.KindImage {
		inboxData = res.Images[0]
	} else {
		inboxData = res.Original
		// A text file is kept as the decoded text only; documents keep the original.
		if res.Kind != attach.KindText {
			ext := strings.ToLower(filepath.Ext(res.Name))
			if rec.Path, err = write(res.Original, ext); err != nil {
				cleanup()
				return err
			}
			rec.StoredBytes += int64(len(res.Original))
		}
		if res.Text != "" {
			if rec.TextPath, err = write([]byte(res.Text), ".txt"); err != nil {
				cleanup()
				return err
			}
			rec.StoredBytes += int64(len(res.Text))
		}
	}
	for _, img := range res.Images {
		rel, err := write(img, ".jpg")
		if err != nil {
			cleanup()
			return err
		}
		rec.Images = append(rec.Images, rel)
		rec.StoredBytes += int64(len(img))
	}
	if res.Kind == attach.KindImage {
		rec.Path = rec.Images[0]
		rec.StoredBytes = int64(len(res.Images[0])) // one file serves as both
	}
	id, err := s.AddAttachment(ctx, rec)
	if err != nil {
		cleanup()
		return err
	}
	if r.attach.cfg.Inbox != nil && len(inboxData) > 0 {
		if err := r.attach.cfg.Inbox(ctx, workspace, attach.InboxPath(id, res.Name, res.Kind), inboxData); err != nil {
			r.log.Warn("could not copy an attachment to the command workspace", "err", err)
		}
	}
	return nil
}

func removeFile(p string) error { return os.Remove(p) }
