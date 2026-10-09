// Package cli implements a terminal channel for trying the bot locally.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/authapon/jannyq/internal/channel"
)

// Channel reads lines from In and prints replies to Out.
type Channel struct {
	In          io.Reader
	Out         io.Writer
	Interactive bool // print a prompt before each line

	outMu sync.Mutex // one writer to Out at a time
}

// New returns a CLI channel on stdin/stdout.
func New() *Channel {
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil {
		interactive = fi.Mode()&os.ModeCharDevice != 0
	}
	return &Channel{In: os.Stdin, Out: os.Stdout, Interactive: interactive}
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "cli" }

type responder struct {
	c  *Channel
	mu *sync.Mutex
}

// ResponderFor implements channel.Notifier: whatever the chat id, it is the terminal.
func (c *Channel) ResponderFor(string, bool) (channel.Responder, error) {
	return responder{c: c, mu: &c.outMu}, nil
}

func (r responder) Typing(context.Context) error { return nil }

func (r responder) Send(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := fmt.Fprintf(r.c.Out, "%s\n", text)
	return err
}

// Run implements channel.Channel. Lines are handled one at a time; it
// returns nil at end of input.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error {
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(c.In)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()

	prompt := func() {
		if c.Interactive {
			fmt.Fprint(c.Out, "> ")
		}
	}
	prompt()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if strings.TrimSpace(line) == "" {
				prompt()
				continue
			}
			sink(ctx, channel.Incoming{
				Channel:   "cli",
				ChatID:    "local",
				UserID:    "local",
				Text:      line,
				Addressed: true,
				Responder: responder{c: c, mu: &c.outMu},
			})
			prompt()
		}
	}
}
