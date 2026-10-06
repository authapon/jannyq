package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func load(t *testing.T, args []string, e map[string]string) (*Config, error) {
	t.Helper()
	return Load(args, env(e), io.Discard)
}

func TestDefaultsAndRequiredModel(t *testing.T) {
	if _, err := load(t, nil, nil); err == nil || !strings.Contains(err.Error(), "llm-model") {
		t.Fatalf("model should be required, err = %v", err)
	}
	c, err := load(t, nil, map[string]string{"JANNYQ_LLM_MODEL": "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMProvider != "ollama" || c.LLMBaseURL != "http://localhost:11434" || c.DataDir != "." ||
		c.CompactAfter != 200 || c.ContextSize != 0 || c.Lang != "en" || c.RateLimit != 20 || c.FetchAllowPrivate {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestEnvAndFlagPrecedence(t *testing.T) {
	e := map[string]string{
		"JANNYQ_LLM_MODEL":     "from-env",
		"JANNYQ_CONTEXT_SIZE":  "8192",
		"JANNYQ_LANG":          "th",
		"JANNYQ_LLM_PROVIDER":  "openai",
		"JANNYQ_CLI":           "true",
		"JANNYQ_LLM_TIMEOUT":   "90s",
		"JANNYQ_ALLOWED_USERS": "1, telegram:2 ,",
	}
	c, err := load(t, nil, e)
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMModel != "from-env" || c.ContextSize != 8192 || c.Lang != "th" || !c.CLI ||
		c.LLMTimeout != 90*time.Second || c.LLMBaseURL != "https://api.openai.com/v1" {
		t.Errorf("env not applied: %+v", c)
	}
	if len(c.AllowedUsers) != 2 || c.AllowedUsers[1] != "telegram:2" {
		t.Errorf("allowed users = %q", c.AllowedUsers)
	}

	c, err = load(t, []string{"--llm-model", "from-flag", "--context-size=4096", "--cli=false"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if c.LLMModel != "from-flag" || c.ContextSize != 4096 || c.CLI || c.Lang != "th" {
		t.Errorf("flags must override env: %+v", c)
	}
}

func TestSecretFromFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(f, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := load(t, nil, map[string]string{"JANNYQ_LLM_MODEL": "m", "JANNYQ_TELEGRAM_TOKEN_FILE": f})
	if err != nil || c.TelegramToken != "s3cret" {
		t.Fatalf("token=%q err=%v", c.TelegramToken, err)
	}
	// plain env wins over the file
	c, _ = load(t, nil, map[string]string{"JANNYQ_LLM_MODEL": "m", "JANNYQ_TELEGRAM_TOKEN": "direct", "JANNYQ_TELEGRAM_TOKEN_FILE": f})
	if c.TelegramToken != "direct" {
		t.Errorf("token = %q", c.TelegramToken)
	}
	if _, err := load(t, nil, map[string]string{"JANNYQ_LLM_MODEL": "m", "JANNYQ_LLM_API_KEY_FILE": "/nonexistent"}); err == nil {
		t.Error("unreadable secret file must fail")
	}
}

func TestSystemPromptFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "prompt.txt")
	_ = os.WriteFile(f, []byte("Be brief.\n"), 0o600)
	c, err := load(t, []string{"--system-prompt-file", f}, map[string]string{"JANNYQ_LLM_MODEL": "m", "JANNYQ_SYSTEM_PROMPT": "Be kind."})
	if err != nil || c.SystemPrompt != "Be kind.\n\nBe brief." {
		t.Fatalf("prompt = %q err = %v", c.SystemPrompt, err)
	}
}

func TestInvalidValues(t *testing.T) {
	base := map[string]string{"JANNYQ_LLM_MODEL": "m"}
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"bad int env":      {map[string]string{"JANNYQ_CONTEXT_SIZE": "lots"}, nil, "JANNYQ_CONTEXT_SIZE"},
		"bad duration":     {map[string]string{"JANNYQ_LLM_TIMEOUT": "5 minutes"}, nil, "JANNYQ_LLM_TIMEOUT"},
		"bad bool":         {map[string]string{"JANNYQ_CLI": "maybe"}, nil, "JANNYQ_CLI"},
		"bad provider":     {map[string]string{"JANNYQ_LLM_PROVIDER": "bing"}, nil, "llm-provider"},
		"bad group reply":  {nil, []string{"--group-reply=sometimes"}, "group-reply"},
		"bad lang mode":    {nil, []string{"--lang-mode=x"}, "lang-mode"},
		"keep >= after":    {nil, []string{"--compact-after=10", "--compact-keep=10"}, "compact-keep"},
		"bad ratio":        {nil, []string{"--compact-ratio=1.5"}, "compact-ratio"},
		"negative context": {nil, []string{"--context-size=-1"}, "context-size"},
		"bad log level":    {nil, []string{"--log-level=loud"}, "log-level"},
	} {
		e := map[string]string{}
		for k, v := range base {
			e[k] = v
		}
		for k, v := range tc.env {
			e[k] = v
		}
		if _, err := load(t, tc.args, e); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tc.want)
		}
	}
}

func TestHelp(t *testing.T) {
	var sb strings.Builder
	_, err := Load([]string{"--help"}, env(nil), &sb)
	if !errors.Is(err, ErrHelp) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"JANNYQ_LLM_MODEL", "JANNYQ_TELEGRAM_TOKEN_FILE", "--searxng-url"} {
		if !strings.Contains(sb.String(), want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}
