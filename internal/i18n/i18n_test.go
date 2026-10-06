package i18n

import "testing"

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
