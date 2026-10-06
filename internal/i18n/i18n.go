// Package i18n provides the bot's fixed user-facing messages in several
// languages. Model replies are not translated here; the agent instructs the
// model which language to use.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

const fallbackLang = "en"

// Translator renders messages for one language, falling back to English.
type Translator struct {
	lang     string
	msgs     map[string]string
	fallback map[string]string
}

// New returns a Translator for lang (e.g. "th", "en-US"). Unknown languages
// use English for fixed messages.
func New(lang string) *Translator {
	lang = Normalize(lang)
	return &Translator{lang: lang, msgs: load(lang), fallback: load(fallbackLang)}
}

// Lang returns the normalised language code.
func (t *Translator) Lang() string { return t.lang }

// T returns the message for key, formatted with args. A missing key renders
// as the key itself so problems are visible rather than silent.
func (t *Translator) T(key string, args ...any) string {
	s, ok := t.msgs[key]
	if !ok {
		s, ok = t.fallback[key]
	}
	if !ok {
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

// Normalize lower-cases a language tag and drops the region ("th-TH" → "th").
func Normalize(lang string) string {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if i := strings.IndexAny(lang, "-_"); i > 0 {
		lang = lang[:i]
	}
	if lang == "" {
		return fallbackLang
	}
	return lang
}

func load(lang string) map[string]string {
	b, err := localeFS.ReadFile("locales/" + lang + ".json")
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]string{}
	}
	return m
}

var languageNames = map[string]string{
	"en": "English", "th": "Thai", "zh": "Chinese", "ja": "Japanese", "ko": "Korean",
	"vi": "Vietnamese", "id": "Indonesian", "ms": "Malay", "tl": "Filipino",
	"es": "Spanish", "fr": "French", "de": "German", "pt": "Portuguese",
	"it": "Italian", "ru": "Russian", "ar": "Arabic", "hi": "Hindi", "tr": "Turkish",
	"nl": "Dutch", "pl": "Polish", "uk": "Ukrainian", "sv": "Swedish",
}

// LanguageName returns the English name of a language code for use in model
// prompts. Unknown codes are returned unchanged so a full name such as
// "Brazilian Portuguese" also works.
func LanguageName(lang string) string {
	if n, ok := languageNames[Normalize(lang)]; ok && !strings.ContainsAny(lang, " ") {
		return n
	}
	return strings.TrimSpace(lang)
}
