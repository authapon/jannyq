//go:build unix

package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef-test-token"

func newPair(t *testing.T, tweak func(*ExecConfig)) (*Executor, *Client, *httptest.Server) {
	t.Helper()
	e := newExec(t, tweak)
	srv, err := NewServer(e, testToken, e.cfg.MaxTimeout, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return e, &Client{BaseURL: ts.URL, Token: testToken, HTTP: ts.Client()}, ts
}

func TestClientServerRoundTrip(t *testing.T) {
	e, c, _ := newPair(t, func(cfg *ExecConfig) { cfg.Network = true })
	ws := WorkspaceID("chat")

	res, err := c.Run(bg, Request{Workspace: ws, Command: "echo hi; echo oops >&2; exit 2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 2 || !strings.Contains(res.Output, "hi") || !strings.Contains(res.Output, "oops") || res.DurationMS < 0 {
		t.Errorf("res = %+v", res)
	}

	info, err := c.Info(bg)
	if err != nil || !info.Network || info.MaxTimeout != seconds(e.cfg.MaxTimeout) {
		t.Errorf("info = %+v err = %v", info, err)
	}

	if _, err := c.Run(bg, Request{Workspace: ws, Command: "echo keep > f"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Reset(bg, ws); err != nil {
		t.Fatal(err)
	}
	if res, _ := c.Run(bg, Request{Workspace: ws, Command: "ls"}); strings.Contains(res.Output, "f") {
		t.Errorf("reset did not clear the workspace: %q", res.Output)
	}
}

func TestServerRejectsBadTokens(t *testing.T) {
	_, c, ts := newPair(t, nil)
	for _, tok := range []string{"", "wrong", testToken + "x", strings.ToUpper(testToken)} {
		bad := &Client{BaseURL: c.BaseURL, Token: tok, HTTP: ts.Client()}
		if _, err := bad.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "echo pwned"}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("token %q: err = %v", tok, err)
		}
		if _, err := bad.Info(bg); err == nil {
			t.Errorf("token %q accepted for /info", tok)
		}
		if err := bad.Reset(bg, WorkspaceID("a")); err == nil {
			t.Errorf("token %q accepted for reset", tok)
		}
	}
	// no Authorization header at all, and the health check stays open
	resp, err := http.Post(ts.URL+"/run", "application/json", strings.NewReader(`{"workspace":"`+WorkspaceID("a")+`","command":"echo pwned"}`))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated run: %v %v", resp, err)
	}
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("healthz: %v %v", resp, err)
	}
}

func TestServerNeedsAStrongToken(t *testing.T) {
	e := newExec(t, nil)
	for _, tok := range []string{"", "short", "123456789012345"} {
		if _, err := NewServer(e, tok, time.Minute, nil); err == nil {
			t.Errorf("token %q accepted", tok)
		}
	}
}

func TestServerMapsErrors(t *testing.T) {
	_, c, ts := newPair(t, func(cfg *ExecConfig) { cfg.MaxConcurrent = 1 })

	if _, err := c.Run(bg, Request{Workspace: "nope", Command: "true"}); err == nil || !strings.Contains(err.Error(), "invalid workspace") {
		t.Errorf("invalid workspace: %v", err)
	}
	if _, err := c.Run(bg, Request{Workspace: WorkspaceID("a"), Command: ""}); err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Errorf("empty command: %v", err)
	}

	go func() { _, _ = c.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "sleep 1"}) }()
	time.Sleep(300 * time.Millisecond)
	if _, err := c.Run(bg, Request{Workspace: WorkspaceID("b"), Command: "true"}); !errors.Is(err, ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/run", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err := ts.Client().Do(req)
	if err != nil || r.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: %v %v", r, err)
	}
	big := strings.NewReader(`{"command":"` + strings.Repeat("a", MaxCommandBytes*2) + `"}`)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/run", big)
	req.Header.Set("Authorization", "Bearer "+testToken)
	r, err = ts.Client().Do(req)
	if err != nil || r.StatusCode != http.StatusBadRequest {
		t.Errorf("oversized body: %v %v", r, err)
	}
}

func TestClientWhenServerIsDown(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1", Token: testToken}
	if _, err := c.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "true"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
	if _, err := c.Info(bg); !errors.Is(err, ErrUnavailable) {
		t.Errorf("info err = %v", err)
	}
}

func TestClientContextCancel(t *testing.T) {
	_, c, _ := newPair(t, nil)
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	defer cancel()
	_, err := c.Run(ctx, Request{Workspace: WorkspaceID("a"), Command: "sleep 30"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}
