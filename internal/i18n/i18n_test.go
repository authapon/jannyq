package i18n

import (
	"strings"
	"testing"
)

func TestTranslate(t *testing.T) {
	th, en := New("th-TH"), New("en")
	if th.Lang() != "th" {
		t.Errorf("lang = %q", th.Lang())
	}
	if th.T("reset_done") == en.T("reset_done") {
		t.Error("thai should differ from english")
	}
	if got := New("fr").T("reset_done"); got != en.T("reset_done") {
		t.Errorf("unknown language should fall back to English, got %q", got)
	}
	if got := en.T("no_such_key"); got != "no_such_key" {
		t.Errorf("missing key = %q", got)
	}
	if New("").Lang() != "en" {
		t.Error("empty lang should default to en")
	}
}

func TestCatalogsHaveSameKeys(t *testing.T) {
	en, th := load("en"), load("th")
	if len(en) == 0 {
		t.Fatal("english catalog empty")
	}
	for k := range en {
		if th[k] == "" {
			t.Errorf("th missing %q", k)
		}
	}
	for k := range th {
		if en[k] == "" {
			t.Errorf("en missing %q", k)
		}
	}
}

func TestLanguageName(t *testing.T) {
	for in, want := range map[string]string{"th": "Thai", "TH-th": "Thai", "en": "English", "xx": "xx", "Brazilian Portuguese": "Brazilian Portuguese"} {
		if got := LanguageName(in); got != want {
			t.Errorf("LanguageName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrefixed(t *testing.T) {
	th := New("th").Prefixed("web_")
	en := New("en").Prefixed("web_")
	if len(th) == 0 || len(th) != len(en) {
		t.Fatalf("th has %d web strings, en has %d", len(th), len(en))
	}
	if th["web_send"] == en["web_send"] || th["web_send"] == "" {
		t.Errorf("thai web_send = %q", th["web_send"])
	}
	for k := range th {
		if !strings.HasPrefix(k, "web_") {
			t.Errorf("unexpected key %q", k)
		}
	}
	// a language without a catalog falls back to English
	if fr := New("fr").Prefixed("web_"); fr["web_send"] != en["web_send"] {
		t.Errorf("fr web_send = %q", fr["web_send"])
	}
}
