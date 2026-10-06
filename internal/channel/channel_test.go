package channel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplit(t *testing.T) {
	if got := Split("  ", 10); got != nil {
		t.Errorf("blank → %v", got)
	}
	if got := Split("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("short → %v", got)
	}

	para := strings.Repeat("a", 60) + "\n\n" + strings.Repeat("b", 60)
	got := Split(para, 100)
	if len(got) != 2 || got[0] != strings.Repeat("a", 60) || got[1] != strings.Repeat("b", 60) {
		t.Errorf("paragraph split: %q", got)
	}

	words := strings.Repeat("word ", 50)
	for _, p := range Split(words, 40) {
		if utf8.RuneCountInString(p) > 40 || strings.HasPrefix(p, " ") || strings.HasSuffix(p, " ") {
			t.Errorf("bad part %q", p)
		}
	}

	// Thai has no spaces: hard cut on rune boundaries, nothing lost
	thai := strings.Repeat("สวัสดี", 100)
	parts := Split(thai, 50)
	var joined strings.Builder
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 50 || !utf8.ValidString(p) {
			t.Fatalf("bad thai part (%d runes)", utf8.RuneCountInString(p))
		}
		joined.WriteString(p)
	}
	if joined.String() != thai {
		t.Error("content lost while splitting")
	}
}
