package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/backup"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/metrics"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/trigger"
)

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestMetricsServer(t *testing.T) {
	addr := freeAddr(t)
	cfg := &config.Config{MetricsListen: addr, MetricsToken: "sekret-token"}
	reg, inst := newMetrics(cfg, "9.9.9")
	inst.HTTPRequests.Inc("2xx")
	sessions := session.NewManager(t.TempDir(), 4, 4)
	defer sessions.Close()
	registerGauges(reg, sessions, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveMetrics(ctx, cfg, reg, quiet()) }()
	get := func(path, auth string) (int, string) {
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ {
			req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/metrics", ""); code != 401 {
		t.Errorf("without the token: %d", code)
	}
	code, body := get("/metrics", "Bearer sekret-token")
	if code != 200 || !strings.Contains(body, `jannyq_http_requests_total{class="2xx"} 1`) || !strings.Contains(body, "jannyq_open_sessions 0") ||
		!strings.Contains(body, `jannyq_build_info{version="9.9.9"`) {
		t.Errorf("%d\n%s", code, body)
	}
	if code, _ := get("/other", "Bearer sekret-token"); code != 404 {
		t.Errorf("only /metrics is served: %d", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the metrics server did not stop")
	}
}

func TestMetricsAreOffByDefault(t *testing.T) {
	reg, inst := newMetrics(&config.Config{}, "x")
	if reg != nil {
		t.Error("a registry was created although metrics are off")
	}
	inst.ToolCalls.Inc("a", "ok") // no-op, no panic
}

func TestScheduledBackupsAreMadeAndPruned(t *testing.T) {
	data, out := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "web_secret"), []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{DataDir: data, BackupDir: out, BackupInterval: 60 * time.Millisecond, BackupKeep: 2, BackupFiles: true}
	reg := metrics.New()
	inst := metrics.NewInstruments(reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runBackupsEvery(ctx, cfg, "t", inst, reg, quiet(), 10*time.Millisecond, 50*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(reg.Render(), `jannyq_backups_total{result="ok"} 3`) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	list, _ := backup.List(out)
	if len(list) == 0 || len(list) > 2 {
		t.Errorf("%d backups kept: %v", len(list), list)
	}
	if !strings.Contains(reg.Render(), `jannyq_backups_total{result="ok"}`) {
		t.Errorf("%s", reg.Render())
	}
	if strings.Contains(reg.Render(), "jannyq_last_backup_timestamp_seconds 0\n") || !strings.Contains(reg.Render(), "jannyq_last_backup_timestamp_seconds 1") {
		t.Errorf("the time of the last backup is not exported:\n%s", reg.Render())
	}
}

func TestBackupFailureIsCountedAndRetried(t *testing.T) {
	cfg := &config.Config{DataDir: filepath.Join(t.TempDir(), "missing"), BackupDir: t.TempDir(), BackupInterval: time.Hour, BackupKeep: 2}
	reg := metrics.New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runBackupsEvery(ctx, cfg, "t", metrics.NewInstruments(reg), reg, quiet(), 5*time.Millisecond, 20*time.Millisecond)
		close(done)
	}()
	// at least two failures: the retry runs every few milliseconds, so the count
	// may step over any exact number between two looks
	failures := func() int {
		var n int
		for _, line := range strings.Split(reg.Render(), "\n") {
			if rest, ok := strings.CutPrefix(line, `jannyq_backups_total{result="error"} `); ok {
				n, _ = strconv.Atoi(rest)
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && failures() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if failures() < 2 {
		t.Errorf("failures are not retried:\n%s", reg.Render())
	}
}

func TestRetentionLoop(t *testing.T) {
	dir := t.TempDir()
	sessions := session.NewManager(dir, 4, 4)
	defer sessions.Close()
	ctx := context.Background()
	if err := sessions.With(ctx, "telegram", "old", func(s *session.Session) error {
		return s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "x"})
	}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * 24 * time.Hour)
	entries, _ := os.ReadDir(filepath.Join(dir, "telegram"))
	for _, f := range []string{"session.db", "session.db-wal"} {
		_ = os.Chtimes(filepath.Join(dir, "telegram", entries[0].Name(), f), past, past)
	}
	reg := metrics.New()
	cfg := &config.Config{RetentionDays: 7}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runRetentionEvery(rctx, cfg, sessions, metrics.NewInstruments(reg), quiet(), 5*time.Millisecond, time.Hour)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sessions.Exists("telegram", "old") {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if sessions.Exists("telegram", "old") || !strings.Contains(reg.Render(), `jannyq_retention_deleted_total{what="chats"} 1`) {
		t.Errorf("the idle chat survived or was not counted:\n%s", reg.Render())
	}
}

func TestReadyz(t *testing.T) {
	dir := t.TempDir()
	get := func(h http.Handler) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := get(readyHandler(dir, nil)); code != 200 || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("%d %s", code, body)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe left files behind: %v", entries)
	}
	if code, body := get(readyHandler(filepath.Join(dir, "gone"), nil)); code != 503 || !strings.Contains(body, "not writable") {
		t.Errorf("%d %s", code, body)
	}
	// the knowledge database is part of it when there is one
	data := t.TempDir()
	cfg := &config.Config{DataDir: data, KnowledgeDir: t.TempDir(), PDFEngine: "native", OCRLangs: "off", Lang: "en",
		KnowledgeMaxFileMB: 5, KnowledgePDFPages: 10, KnowledgeChunkChars: 500, KnowledgeOverlap: 50, KnowledgeInterval: time.Second}
	k, err := newKnowledge(context.Background(), cfg, nil, metrics.NewInstruments(nil), quiet())
	if err != nil {
		t.Fatal(err)
	}
	h := readyHandler(data, k)
	if code, body := get(h); code != 200 || !strings.Contains(body, `"knowledge":"ok"`) {
		t.Errorf("%d %s", code, body)
	}
	k.store.Close()
	k2, _ := newKnowledge(context.Background(), cfg, nil, metrics.NewInstruments(nil), quiet())
	k2.store.Close() // a database that has gone away
	if code, body := get(readyHandler(data, k2)); code != 503 || !strings.Contains(body, "unavailable") {
		t.Errorf("%d %s", code, body)
	}
}

func TestDataDirectoryPermissionWarning(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o777)
	checkDataDir(dir, log)
	if !strings.Contains(buf.String(), "can be read by every user") || !strings.Contains(buf.String(), "chmod 750") {
		t.Errorf("no warning for a world-accessible directory: %q", buf.String())
	}
	buf.Reset()
	_ = os.Chmod(dir, 0o750)
	checkDataDir(dir, log)
	checkDataDir(filepath.Join(dir, "missing"), log)
	if buf.Len() != 0 {
		t.Errorf("a private directory was reported: %q", buf.String())
	}
}

func TestTriggersOfDeletedChatsAreForgotten(t *testing.T) {
	dir := t.TempDir()
	sessions := session.NewManager(dir, 4, 4)
	defer sessions.Close()
	ctx := context.Background()
	for _, chat := range []string{"old", "kept"} {
		if err := sessions.With(ctx, "telegram", chat, func(s *session.Session) error {
			return s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "x"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-10 * 24 * time.Hour)
	entries, _ := os.ReadDir(filepath.Join(dir, "telegram"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "old") {
			for _, f := range []string{"session.db", "session.db-wal"} {
				_ = os.Chtimes(filepath.Join(dir, "telegram", e.Name(), f), past, past)
			}
		}
	}
	cfg := &config.Config{DataDir: t.TempDir(), Triggers: true, TriggerGrace: time.Hour}
	ts, err := newTriggers(cfg, time.UTC, metrics.NewInstruments(nil), quiet())
	if err != nil || ts == nil {
		t.Fatal(err)
	}
	defer ts.store.Close()
	for _, chat := range []string{"old", "kept"} {
		if _, err := ts.store.Create(ctx, trigger.Trigger{Channel: "telegram", ChatID: chat, OwnerID: "1", Mode: trigger.ModeRemind, Text: "x",
			Cron: "0 7 * * *", Zone: "UTC", Next: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runRetentionEvery(rctx, &config.Config{RetentionDays: 7}, sessions, metrics.NewInstruments(nil), quiet(), 5*time.Millisecond, time.Hour,
			func() { ts.forgetChats(sessions, quiet()) })
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l, _ := ts.store.List(ctx, "telegram", "old"); len(l) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if l, _ := ts.store.List(ctx, "telegram", "old"); len(l) != 0 {
		t.Error("the trigger of a deleted chat survived")
	}
	if l, _ := ts.store.List(ctx, "telegram", "kept"); len(l) != 1 {
		t.Error("the trigger of a chat that is still there was deleted")
	}
	// off by configuration: nothing is opened
	if off, err := newTriggers(&config.Config{DataDir: t.TempDir()}, time.UTC, metrics.NewInstruments(nil), quiet()); off != nil || err != nil {
		t.Errorf("triggers off: %v %v", off, err)
	}
}
