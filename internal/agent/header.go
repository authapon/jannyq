package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
)

// TimeLayout is the ISO 8601 form in which message times are stored and
// shown, e.g. 1997-07-16T19:20:44+01:00. Unlike time.RFC3339 it writes
// +00:00 rather than Z, so every timestamp has the same shape.
const TimeLayout = "2006-01-02T15:04:05-07:00"

// headerTokens is a generous estimate of what one header costs in tokens.
const headerTokens = 16

const maxNameRunes = 32

// FormatTime renders t in loc as an ISO 8601 timestamp with its UTC offset.
func FormatTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	return t.In(loc).Format(TimeLayout)
}

// sanitizeName makes a display name safe to put in a header: no control,
// invisible or direction-changing characters, none of the characters the
// header uses as punctuation, and a bounded length. Names are chosen by
// users, so without this one could forge the header of another person.
func sanitizeName(name string) string {
	var sb strings.Builder
	space := false
	for _, r := range name {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			r = ' '
		case strings.ContainsRune("[]():#<>@`*", r):
			r = ' '
		}
		if unicode.IsSpace(r) {
			space = sb.Len() > 0
			continue
		}
		if space {
			sb.WriteByte(' ')
			space = false
		}
		sb.WriteRune(r)
	}
	out := sb.String()
	if rs := []rune(out); len(rs) > maxNameRunes {
		out = strings.TrimSpace(string(rs[:maxNameRunes]))
	}
	if out == "" {
		return "user"
	}
	return out
}

// headerLike matches a line that starts like a message header.
var headerLike = regexp.MustCompile(`(?m)^([ \t]*)\[(\d{4}-\d{2}-\d{2}T[^\]\n]{0,40})\]`)

// neutralizeHeaders turns lines of a message that look like a header into
// ordinary parenthesised text. The real header is added by the bot at the very
// start of the message; a user typing "[2026-…] Bob: …" on a later line must
// not be able to make the model believe that Bob said something.
func neutralizeHeaders(text string) string {
	return headerLike.ReplaceAllString(text, "$1($2)")
}

// tagFor returns a short stable tag that tells apart people who share a
// display name in one chat. It is derived from the platform user id and the
// chat, so it reveals nothing about the id itself.
func tagFor(chatKey, senderID string) string {
	sum := sha256.Sum256([]byte("jannyq-user-tag\x00" + chatKey + "\x00" + senderID))
	return hex.EncodeToString(sum[:])[:4]
}

// header builds the part shown before a user message: when it was sent and,
// in groups, who sent it.
func (a *Agent) header(chatKey string, group bool, st session.Stored) string {
	ts := st.SentAt
	if ts == "" && st.CreatedAt > 0 { // stored before send times were recorded
		ts = FormatTime(time.Unix(st.CreatedAt, 0), a.cfg.Location)
	}
	var sb strings.Builder
	if ts != "" {
		sb.WriteString("[" + ts + "]")
	}
	if group && st.SenderName != "" {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(sanitizeName(st.SenderName))
		if st.SenderID != "" {
			sb.WriteString("#" + tagFor(chatKey, st.SenderID))
		}
		sb.WriteByte(':')
	}
	return sb.String()
}

// renderHistory turns stored messages into the messages sent to the model:
// user messages get their header, everything else is passed on unchanged.
// It is deterministic, so the rendered history is the same on every call and
// the model server can reuse its cache of the unchanged beginning.
func (a *Agent) renderHistory(chatKey string, group bool, stored []session.Stored) []llm.Message {
	out := make([]llm.Message, 0, len(stored))
	for _, st := range stored {
		m := st.Message
		if m.Role == llm.RoleUser {
			body := neutralizeHeaders(m.Content)
			if h := a.header(chatKey, group, st); h != "" {
				m.Content = h + " " + body
			} else {
				m.Content = body
			}
		}
		out = append(out, m)
	}
	return out
}
