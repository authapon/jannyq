// Package channel defines how chat platforms plug into the bot.
package channel

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Responder sends output back to the chat a message came from.
type Responder interface {
	// Send delivers text, splitting it as the platform requires.
	Send(ctx context.Context, text string) error
	// Typing shows a "typing…" indicator (best effort).
	Typing(ctx context.Context) error
}

// Incoming is a user message from any channel.
type Incoming struct {
	Channel  string // channel name, e.g. "telegram"
	ChatID   string // conversation: the user in private chats, the group in groups
	UserID   string
	UserName string
	Text     string
	IsGroup  bool
	// Addressed is true when the message is meant for the bot: always in
	// private chats; in groups when the bot is mentioned, replied to, or
	// given a command.
	Addressed bool
	// HasAttachment is true when the message carried a file, photo, etc.
	// that the channel could not turn into text.
	HasAttachment bool
	Responder     Responder
}

// Sink receives incoming messages. It blocks until the message has been
// fully handled; channels that must keep reading call it from a goroutine.
type Sink func(ctx context.Context, in Incoming)

// Channel is a chat platform adapter.
type Channel interface {
	Name() string
	// Run receives messages until ctx is cancelled (returning nil) or a
	// fatal error occurs. It must wait for in-flight sink calls to finish
	// before returning.
	Run(ctx context.Context, sink Sink) error
}

// Split breaks text into parts of at most limit runes, preferring paragraph,
// line and word boundaries.
func Split(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	var parts []string
	rest := []rune(text)
	for len(rest) > limit {
		cut := lastBreak(rest[:limit])
		if cut <= 0 {
			cut = limit
		}
		if part := strings.TrimSpace(string(rest[:cut])); part != "" {
			parts = append(parts, part)
		}
		rest = []rune(strings.TrimLeft(string(rest[cut:]), " \n\t"))
	}
	if len(rest) > 0 {
		parts = append(parts, string(rest))
	}
	return parts
}

// lastBreak finds the best split point inside r, searching the second half
// first for a blank line, then a newline, then a space.
func lastBreak(r []rune) int {
	min := len(r) / 2
	for _, sep := range [][]rune{[]rune("\n\n"), []rune("\n"), []rune(" ")} {
		for i := len(r) - len(sep); i >= min; i-- {
			if string(r[i:i+len(sep)]) == string(sep) {
				return i + len(sep)
			}
		}
	}
	return 0
}
