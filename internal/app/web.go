package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/authapon/jannyq/internal/channel/web"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/session"
)

// webHistory reads a web visitor's stored conversation.
type webHistory struct{ sessions *session.Manager }

func (h webHistory) Recent(ctx context.Context, chatID string, n int) ([]web.Turn, error) {
	var out []web.Turn
	if !h.sessions.Exists("web", chatID) {
		return out, nil // nothing was said yet; do not create a database for a mere page view
	}
	err := h.sessions.Peek("web", chatID, func(s *session.Session) error {
		turns, err := s.Recent(ctx, n)
		for _, t := range turns {
			out = append(out, web.Turn{Role: t.Role, Text: t.Text, Attachments: t.Attachments})
		}
		return err
	})
	return out, err
}

// webSecret returns the key that signs web sessions: the configured one, or
// a random key created on first start and kept in the data directory so that
// visitors stay signed in across restarts.
func webSecret(cfg *config.Config) ([]byte, error) {
	if cfg.WebSecret != "" {
		return []byte(cfg.WebSecret), nil
	}
	path := filepath.Join(cfg.DataDir, "web_secret")
	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); len(s) >= 32 {
			return []byte(s), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return []byte(secret), nil
}
