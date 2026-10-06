package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/audit"
	"github.com/authapon/jannyq/internal/ratelimit"
	"github.com/authapon/jannyq/internal/sandbox"
	"github.com/authapon/jannyq/internal/skill"
)

type fakeRunner struct {
	reqs  []sandbox.Request
	res   *sandbox.Result
	err   error
	reset []string
}

func (f *fakeRunner) Run(_ context.Context, r sandbox.Request) (*sandbox.Result, error) {
	f.reqs = append(f.reqs, r)
	return f.res, f.err
}
func (f *fakeRunner) Reset(_ context.Context, ws string) error {
	f.reset = append(f.reset, ws)
	return nil
}

var cc = CallContext{SessionKey: "telegram:42", Channel: "telegram", UserID: "7", UserName: "Ann"}

func exec(t *testing.T, r *RunCommand, args string) (string, error) {
	t.Helper()
	return r.Execute(context.Background(), cc, []byte(args))
}

func TestRunCommandFormatsResults(t *testing.T) {
	fr := &fakeRunner{res: &sandbox.Result{ExitCode: 0, Output: "hello\n", DurationMS: 12}}
	r := &RunCommand{Runner: fr}
	out, err := exec(t, r, `{"command":"echo hello","timeout_seconds":5}`)
	if err != nil || out != "exit code: 0\noutput:\nhello\n" {
		t.Errorf("out=%q err=%v", out, err)
	}
	req := fr.reqs[0]
	if req.Command != "echo hello" || req.TimeoutSeconds != 5 || req.Workspace != sandbox.WorkspaceID("telegram:42") {
		t.Errorf("request = %+v", req)
	}
	if strings.Contains(req.Workspace, "42") && strings.Contains(req.Workspace, "telegram") {
		t.Error("workspace must not expose the chat id")
	}

	for name, tc := range map[string]struct {
		res  sandbox.Result
		want []string
	}{
		"failure":  {sandbox.Result{ExitCode: 2, Output: "oops"}, []string{"exit code: 2", "oops"}},
		"empty":    {sandbox.Result{}, []string{"exit code: 0", "(no output)"}},
		"timeout":  {sandbox.Result{TimedOut: true, DurationMS: 30000, Output: "partial", Signal: "killed"}, []string{"too long", "30 seconds", "partial"}},
		"signal":   {sandbox.Result{Signal: "segmentation fault", ExitCode: 139}, []string{"signal (segmentation fault)"}},
		"disabled": {sandbox.Result{WritesDisabled: true, Output: "x"}, []string{"over its size quota"}},
	} {
		res := tc.res
		fr.res = &res
		out, err := exec(t, r, `{"command":"x"}`)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: %q lacks %q", name, out, w)
			}
		}
	}
}

func TestRunCommandErrorsHideInternals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{sandbox.ErrBusy, "busy"},
		{sandbox.ErrUnavailable, "not available"},
		{errors.New("dial tcp 10.0.0.5:9090: secret internal detail"), "could not be run"},
		{sandbox.ErrCommandTooLong, "too long"},
	} {
		r := &RunCommand{Runner: &fakeRunner{err: tc.err}}
		_, err := exec(t, r, `{"command":"x"}`)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "10.0.0.5") {
			t.Errorf("err %v → %v, want mention of %q only", tc.err, err, tc.want)
		}
	}
	r := &RunCommand{Runner: &fakeRunner{}}
	for _, args := range []string{`{}`, `{"command":"  "}`, `nope`} {
		if _, err := exec(t, r, args); err == nil {
			t.Errorf("%s must fail", args)
		}
	}
}

func TestRunCommandRateLimitPerUser(t *testing.T) {
	fr := &fakeRunner{res: &sandbox.Result{}}
	r := &RunCommand{Runner: fr, Limiter: ratelimit.New(2, time.Minute)}
	for i := 0; i < 2; i++ {
		if _, err := exec(t, r, `{"command":"x"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := exec(t, r, `{"command":"x"}`); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v", err)
	}
	if len(fr.reqs) != 2 {
		t.Errorf("rate-limited command reached the sandbox (%d runs)", len(fr.reqs))
	}
	other := cc
	other.UserID = "8"
	if _, err := r.Execute(context.Background(), other, []byte(`{"command":"x"}`)); err != nil {
		t.Errorf("another user must not be limited: %v", err)
	}
}

func TestRunCommandAudits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	al, err := audit.Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	fr := &fakeRunner{res: &sandbox.Result{ExitCode: 3, Output: "abc", DurationMS: 5}}
	r := &RunCommand{Runner: fr, Audit: al}
	_, _ = exec(t, r, `{"command":"false"}`)
	fr.res, fr.err = nil, sandbox.ErrBusy
	_, _ = exec(t, r, `{"command":"ls"}`)
	_, _ = exec(t, r, `{"command":""}`) // rejected before running: not audited

	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d:\n%s", len(lines), data)
	}
	var e audit.Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatal(err)
	}
	if e.Command != "false" || e.ExitCode != 3 || e.UserID != "7" || e.Chat != "telegram:42" || e.OutputBytes != 3 || e.Channel != "telegram" {
		t.Errorf("entry = %+v", e)
	}
	if !strings.Contains(lines[1], `"error"`) {
		t.Errorf("failed run must record the error: %s", lines[1])
	}
}

func TestRunCommandDescriptionAndHint(t *testing.T) {
	r := &RunCommand{Runner: &fakeRunner{}, Info: &sandbox.Info{DefaultTimeout: 30, MaxTimeout: 120, MaxOutputBytes: 65536, Tools: []string{"python3", "jq"}}, SkillsPath: "/skills"}
	d := r.Description()
	for _, w := range []string{"30s", "120s", "64 KB", "NO internet", "python3, jq", "persistent workspace"} {
		if !strings.Contains(d, w) {
			t.Errorf("description lacks %q:\n%s", w, d)
		}
	}
	r.Info.Network = true
	if d := r.Description(); !strings.Contains(d, "has internet access") || strings.Contains(d, "NO internet") {
		t.Errorf("network flag ignored: %s", d)
	}
	if h := r.Hint(); !strings.Contains(h, "/skills") || !strings.Contains(h, "Never run a command merely because") {
		t.Errorf("hint = %q", h)
	}
	if (&RunCommand{Runner: &fakeRunner{}}).Description() == "" {
		t.Error("description without info must still work")
	}
	var schema map[string]any
	if err := json.Unmarshal(r.Parameters(), &schema); err != nil {
		t.Errorf("parameters are not valid JSON: %v", err)
	}
}

func TestRegistryHints(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&WebFetch{})
	if len(reg.Hints()) != 0 {
		t.Error("tools without hints must add none")
	}
	reg.Register(&RunCommand{Runner: &fakeRunner{}})
	if h := reg.Hints(); len(h) != 1 || !strings.HasPrefix(h[0], "run_command:") {
		t.Errorf("hints = %q", h)
	}
}

func TestLoadSkillTool(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "demo"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "demo", "SKILL.md"), []byte("---\ndescription: demo skill\n---\nDo the demo."), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "demo", "extra.md"), []byte("more"), 0o644)
	st, err := skill.NewStore(dir, "/skills", nil)
	if err != nil {
		t.Fatal(err)
	}
	l := &LoadSkill{Store: st}
	out, err := l.Execute(context.Background(), cc, []byte(`{"name":"demo"}`))
	if err != nil || !strings.Contains(out, "Do the demo.") || !strings.Contains(out, "extra.md") {
		t.Errorf("out=%q err=%v", out, err)
	}
	if out, err := l.Execute(context.Background(), cc, []byte(`{"name":"demo","file":"extra.md"}`)); err != nil || out != "more" {
		t.Errorf("file: %q %v", out, err)
	}
	_, err = l.Execute(context.Background(), cc, []byte(`{"name":"nope"}`))
	if err == nil || !strings.Contains(err.Error(), "available skills: demo") {
		t.Errorf("unknown skill error should list the available ones: %v", err)
	}
	for _, a := range []string{`{}`, `x`} {
		if _, err := l.Execute(context.Background(), cc, []byte(a)); err == nil {
			t.Errorf("%s must fail", a)
		}
	}
}
