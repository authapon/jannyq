// Package channel defines how chat platforms plug into the bot.
package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Responder sends output back to the chat a message came from.
type Responder interface {
	// Send delivers text, splitting it as the platform requires.
	Send(ctx context.Context, text string) error
	// Typing shows a "typing…" indicator (best effort).
	Typing(ctx context.Context) error
}

// ErrTooLarge is returned by Attachment.Fetch for a file over the limit.
var ErrTooLarge = errors.New("channel: file too large")

// Attachment is a file that came with a message. The content is fetched on
// demand, so the router can turn a message away before any download happens.
type Attachment struct {
	Name string // file name, if the platform gives one
	MIME string // declared type, if any; not trusted
	Size int64  // declared size in bytes, 0 if unknown
	// Fetch returns the content, failing if it is larger than max bytes.
	Fetch func(ctx context.Context, max int64) ([]byte, error)
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
	// Attachments are the files the message carried, if the channel can
	// provide them.
	Attachments []Attachment
	// HasAttachment is true when the message carried something the channel
	// could not provide (a video, a sticker, ...) and nothing else.
	HasAttachment bool
	// ReceivedAt is when the message was sent. Channels set it when the
	// platform says so; otherwise the router uses the time it arrived.
	ReceivedAt time.Time
	// Accepted, when set, is called by the router as soon as the message has
	// been taken in: stored, or turned away. It happens before any slow work
	// such as asking the model, so channels that handle messages concurrently
	// use it (see Orderer) to keep each chat's messages in the order they were sent.
	Accepted func()
	// Origin identifies where an anonymous user comes from (the client IP of
	// the web chat), so that limits cannot be dodged by creating new
	// identities. Empty for platforms with real accounts.
	Origin    string
	Responder Responder
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

// Orderer keeps the messages of one chat in the order they arrived when a
// channel handles them in separate goroutines. Call Enter for each message,
// in arrival order and before starting its goroutine; the goroutine waits on
// the returned channel, passes accepted as Incoming.Accepted and also calls
// it when it is done, so a message that is never accepted cannot hold up the
// ones behind it.
type Orderer struct {
	mu   sync.Mutex
	tail map[string]chan struct{}
}

// NewOrderer returns an empty Orderer.
func NewOrderer() *Orderer { return &Orderer{tail: map[string]chan struct{}{}} }

// Enter registers a message of the chat identified by key. wait is nil when
// no earlier message of the chat is still being accepted. accepted may be
// called any number of times.
func (o *Orderer) Enter(key string) (wait <-chan struct{}, accepted func()) {
	cur := make(chan struct{})
	o.mu.Lock()
	prev := o.tail[key]
	o.tail[key] = cur
	o.mu.Unlock()

	var once sync.Once
	accepted = func() {
		once.Do(func() {
			close(cur)
			o.mu.Lock()
			if o.tail[key] == cur { // nobody is queued behind us
				delete(o.tail, key)
			}
			o.mu.Unlock()
		})
	}
	if prev == nil {
		return nil, accepted
	}
	return prev, accepted
}
