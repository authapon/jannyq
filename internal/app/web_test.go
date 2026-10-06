package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
)

func TestWebSecretIsCreatedOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir}
	first, err := webSecret(cfg)
	if err != nil || len(first) < 32 {
		t.Fatalf("secret = %q err = %v", first, err)
	}
	fi, err := os.Stat(filepath.Join(dir, "web_secret"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", fi, err)
	}
	again, err := webSecret(cfg)
	if err != nil || string(again) != string(first) {
		t.Errorf("the secret changed between starts: %q vs %q", first, again)
	}

	// a configured secret wins and creates no file
	other := t.TempDir()
	got, err := webSecret(&config.Config{DataDir: other, WebSecret: "configured-secret-123"})
	if err != nil || string(got) != "configured-secret-123" {
		t.Errorf("got %q err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(other, "web_secret")); err == nil {
		t.Error("a file was written although the secret is configured")
	}

	// a truncated file is not trusted
	bad := t.TempDir()
	_ = os.WriteFile(filepath.Join(bad, "web_secret"), []byte("short\n"), 0o600)
	fixed, err := webSecret(&config.Config{DataDir: bad})
	if err != nil || len(fixed) < 32 {
		t.Errorf("weak stored secret reused: %q %v", fixed, err)
	}
}

func TestWebHistoryReadsTheChatDatabase(t *testing.T) {
	m := session.NewManager(t.TempDir(), 4, 4)
	defer m.Close()
	ctx := context.Background()
	_ = m.With(ctx, "web", "visitor1", func(s *session.Session) error {
		return s.Append(ctx,
			llm.Message{Role: llm.RoleUser, Content: "hi"},
			llm.Message{Role: llm.RoleAssistant, Content: "hello"})
	})
	_ = m.With(ctx, "web", "visitor2", func(s *session.Session) error {
		return s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "private"})
	})
	h := webHistory{m}
	turns, err := h.Recent(ctx, "visitor1", 10)
	if err != nil || len(turns) != 2 || turns[0].Role != "user" || turns[1].Text != "hello" {
		t.Errorf("turns = %+v err = %v", turns, err)
	}
	for _, tn := range turns {
		if strings.Contains(tn.Text, "private") {
			t.Error("history leaked another visitor's messages")
		}
	}
	if none, err := h.Recent(ctx, "stranger", 10); err != nil || len(none) != 0 {
		t.Errorf("stranger = %+v %v", none, err)
	}
}

func TestWebHistoryDoesNotCreateDatabasesForPageViews(t *testing.T) {
	dir := t.TempDir()
	m := session.NewManager(dir, 4, 4)
	defer m.Close()
	h := webHistory{m}
	for i := 0; i < 5; i++ {
		if turns, err := h.Recent(context.Background(), "visitor"+string(rune('a'+i)), 10); err != nil || len(turns) != 0 {
			t.Fatalf("turns = %+v err = %v", turns, err)
		}
	}
	var files int
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Errorf("%d files were created by reading empty histories", files)
	}
	if m.Exists("web", "visitora") {
		t.Error("Exists reports a chat that was never stored")
	}
	_ = m.With(context.Background(), "web", "visitora", func(s *session.Session) error {
		return s.Append(context.Background(), llm.Message{Role: llm.RoleUser, Content: "hi"})
	})
	if !m.Exists("web", "visitora") {
		t.Error("Exists must report a stored chat")
	}
}
