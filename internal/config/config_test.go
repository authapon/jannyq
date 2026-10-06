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

func TestRunCommandSettings(t *testing.T) {
	base := map[string]string{"JANNYQ_LLM_MODEL": "m"}
	with := func(extra map[string]string) map[string]string {
		e := map[string]string{}
		for k, v := range base {
			e[k] = v
		}
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	c, err := load(t, nil, base)
	if err != nil || c.RunCommand != "off" || c.RunRate != 10 || c.SkillsSandboxPath != "/skills" {
		t.Fatalf("defaults: %+v err=%v", c, err)
	}

	tok := "0123456789abcdef0123"
	c, err = load(t, nil, with(map[string]string{"JANNYQ_RUN_COMMAND": "sandbox", "JANNYQ_SANDBOX_URL": "http://sandbox:9090", "JANNYQ_SANDBOX_TOKEN": tok}))
	if err != nil || c.SandboxURL != "http://sandbox:9090" || c.SandboxToken != tok {
		t.Errorf("sandbox mode: %+v err=%v", c, err)
	}

	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"sandbox needs url":   {map[string]string{"JANNYQ_RUN_COMMAND": "sandbox", "JANNYQ_SANDBOX_TOKEN": tok}, nil, "sandbox-url"},
		"sandbox needs token": {map[string]string{"JANNYQ_RUN_COMMAND": "sandbox", "JANNYQ_SANDBOX_URL": "http://s"}, nil, "SANDBOX_TOKEN"},
		"weak token":          {map[string]string{"JANNYQ_RUN_COMMAND": "sandbox", "JANNYQ_SANDBOX_URL": "http://s", "JANNYQ_SANDBOX_TOKEN": "short"}, nil, "16 characters"},
		"host needs ack":      {map[string]string{"JANNYQ_RUN_COMMAND": "host"}, nil, "run-host-unsafe"},
		"unknown mode":        {nil, []string{"--run-command=docker"}, "run-command"},
		"negative rate":       {nil, []string{"--run-rate=-1"}, "run-rate"},
	} {
		if _, err := load(t, tc.args, with(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tc.want)
		}
	}
	if c, err := load(t, []string{"--run-command=host", "--run-host-unsafe"}, base); err != nil || c.RunCommand != "host" {
		t.Errorf("acknowledged host mode: %+v err=%v", c, err)
	}
}

func TestLoadSandbox(t *testing.T) {
	tok := "0123456789abcdef0123"
	c, err := LoadSandbox(nil, env(map[string]string{"JANNYQ_SANDBOX_TOKEN": tok}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9090" || c.WorkDir != "/work" || c.UIDBase != 20000 || c.UIDCount != 40000 || c.MaxTimeout != 120*time.Second ||
		c.Network != "off" || c.QuotaMB != 256 || c.AllowRoot {
		t.Errorf("defaults: %+v", c)
	}

	c, err = LoadSandbox([]string{"--max-timeout=45s", "--network=on", "--quota-mb", "10"},
		env(map[string]string{"JANNYQ_SANDBOX_TOKEN": tok, "JANNYQ_SANDBOX_WORKDIR": "/data/w", "JANNYQ_SANDBOX_MAX_TIMEOUT": "10s"}), io.Discard)
	if err != nil || c.MaxTimeout != 45*time.Second || c.Network != "on" || c.QuotaMB != 10 || c.WorkDir != "/data/w" {
		t.Errorf("overrides: %+v err=%v", c, err)
	}

	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte(tok+"\n"), 0o600)
	if c, err := LoadSandbox(nil, env(map[string]string{"JANNYQ_SANDBOX_TOKEN_FILE": f}), io.Discard); err != nil || c.Token != tok {
		t.Errorf("token file: %+v err=%v", c, err)
	}

	for name, args := range map[string][]string{
		"default > max": {"--default-timeout=5m"},
		"bad network":   {"--network=maybe"},
		"zero quota":    {"--quota-mb=0"},
		"low uid base":  {"--uid-base=0"},
	} {
		if _, err := LoadSandbox(args, env(map[string]string{"JANNYQ_SANDBOX_TOKEN": tok}), io.Discard); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadSandbox(nil, env(nil), io.Discard); err == nil || !strings.Contains(err.Error(), "TOKEN") {
		t.Errorf("missing token: %v", err)
	}
	if _, err := LoadSandbox(nil, env(map[string]string{"JANNYQ_SANDBOX_TOKEN": "short"}), io.Discard); err == nil {
		t.Error("short token accepted")
	}
}

func TestWebAndServerSettings(t *testing.T) {
	base := map[string]string{"JANNYQ_LLM_MODEL": "m"}
	with := func(extra map[string]string) map[string]string {
		e := map[string]string{}
		for k, v := range base {
			e[k] = v
		}
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	c, err := load(t, nil, base)
	if err != nil || c.Web || c.HTTPRate != 300 || c.WebBasePath != "/" || c.WebMaxMessage != 4000 || c.WebRunRate != 3 || c.WebSecureCookies != "auto" {
		t.Fatalf("defaults: %+v err=%v", c, err)
	}
	c, err = load(t, nil, with(map[string]string{
		"JANNYQ_WEB": "true", "JANNYQ_LISTEN": ":8080", "JANNYQ_WEB_BASE_PATH": "/chat/", "JANNYQ_WEB_ACCESS_CODE": "letmein",
		"JANNYQ_TRUSTED_PROXIES": "10.0.0.0/8, 172.28.0.5", "JANNYQ_WEB_ALLOWED_ORIGINS": "https://a.example, https://b.example",
	}))
	if err != nil || !c.Web || c.WebAccessCode != "letmein" || len(c.TrustedProxies) != 2 || len(c.WebAllowedOrigins) != 2 || c.WebBasePath != "/chat/" {
		t.Errorf("web settings: %+v err=%v", c, err)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
		want string
	}{
		"web needs listen":    {nil, []string{"--web"}, "--listen"},
		"bad cookie mode":     {map[string]string{"JANNYQ_LISTEN": ":1"}, []string{"--web", "--web-secure-cookies=maybe"}, "web-secure-cookies"},
		"bad proxy":           {nil, []string{"--trusted-proxies=10.0.0.0/8,nonsense"}, "trusted-proxies"},
		"short web secret":    {map[string]string{"JANNYQ_LISTEN": ":1"}, []string{"--web", "--web-secret=short"}, "web-secret"},
		"huge message limit":  {map[string]string{"JANNYQ_LISTEN": ":1"}, []string{"--web", "--web-max-message=1000000"}, "web-max-message"},
		"bad run rate":        {map[string]string{"JANNYQ_LISTEN": ":1"}, []string{"--web", "--web-run-rate=-5"}, "web-run-rate"},
		"base path with dots": {map[string]string{"JANNYQ_LISTEN": ":1"}, []string{"--web", "--web-base-path=/../x/"}, "web-base-path"},
		"negative http rate":  {nil, []string{"--http-rate=-1"}, "http-rate"},
	} {
		if _, err := load(t, tc.args, with(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tc.want)
		}
	}
	// the web settings are not validated while the web chat is off
	if _, err := load(t, []string{"--web-secure-cookies=maybe"}, base); err != nil {
		t.Errorf("web settings checked although --web is off: %v", err)
	}
}
