package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/attach"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/sandbox"
	"github.com/authapon/jannyq/internal/session"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "attach", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fileSpec struct {
	att     channel.Attachment
	fetches *atomic.Int32
}

func file(name string, data []byte) fileSpec {
	var n atomic.Int32
	return fileSpec{fetches: &n, att: channel.Attachment{Name: name, Size: int64(len(data)),
		Fetch: func(_ context.Context, max int64) ([]byte, error) {
			n.Add(1)
			if int64(len(data)) > max {
				return nil, attach.ErrTooLarge
			}
			return data, nil
		}}}
}

func attachRouter(t *testing.T, f *fakeLLM, acfg agent.Config, c AttachConfig, cfg attach.Config) *Router {
	t.Helper()
	acfg.Model, acfg.CompactAfter, acfg.CompactKeep = "m", 200, 20
	acfg.Attachments = true
	acfg.Vision = cfg.Vision
	r := newRouterWith(t, f, Config{}, acfg)
	c.Processor = attach.New(cfg, nil, attach.NativeEngine{})
	r.SetAttachments(c)
	return r
}

func withFiles(rec *recorder, text string, files ...fileSpec) channel.Incoming {
	in := msg(rec, text)
	for _, f := range files {
		in.Attachments = append(in.Attachments, f.att)
	}
	return in
}

func (r *Router) attachmentsOf(t *testing.T, chat string) []session.Attachment {
	t.Helper()
	var out []session.Attachment
	err := r.sessions.Record("test", chat, func(s *session.Session) error {
		var err error
		out, err = s.ListAttachments(ctx, 100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func lastRequestUser(f *fakeLLM) llm.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last llm.Message
	for _, m := range f.requests[len(f.requests)-1].Messages {
		if m.Role == llm.RoleUser {
			last = m
		}
	}
	return last
}

func TestPhotoWithoutCaption(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{Vision: true})
	rec := &recorder{}
	r.Handle(ctx, withFiles(rec, "", file("IMG_001.png", fixture(t, "alpha.png"))))
	if got := rec.all(); len(got) != 1 || got[0] != "pong" {
		t.Fatalf("sent = %q", got)
	}
	m := lastRequestUser(f)
	if len(m.Images) != 1 || m.Images[0].MIME != "image/jpeg" || !strings.Contains(m.Content, `"IMG_001.png"`) {
		t.Errorf("message = %q, %d images", m.Content, len(m.Images))
	}
	if strings.Contains(m.Content, "  ") {
		t.Errorf("a picture without a caption leaves a double space: %q", m.Content)
	}
	atts := r.attachmentsOf(t, "c1")
	if len(atts) != 1 || atts[0].Kind != attach.KindImage || len(atts[0].Images) != 1 || atts[0].StoredBytes == 0 {
		t.Errorf("attachments = %+v", atts)
	}
}

func TestPDFWithCaption(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	r.Handle(ctx, withFiles(rec, "summarise this", file("report.pdf", fixture(t, "text-en.pdf"))))
	m := lastRequestUser(f)
	for _, want := range []string{"summarise this", "marmalade project shipped on time", "BEGIN ATTACHMENT 1", `"report.pdf"`} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("the model did not get %q: %q", want, m.Content)
		}
	}
	a := r.attachmentsOf(t, "c1")[0]
	if a.Pages != 2 || !a.Inline || a.Path == "" || a.TextPath == "" {
		t.Errorf("attachment = %+v", a)
	}
}

func TestUnreadableFileTellsTheUser(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	// no text: the model is not asked
	r.Handle(ctx, withFiles(rec, "", file("secret.pdf", fixture(t, "encrypted.pdf"))))
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "secret.pdf") || !strings.Contains(got[0], "password") {
		t.Errorf("sent = %q", got)
	}
	if f.calls() != 0 {
		t.Error("the model was called with nothing to answer")
	}
	a := r.attachmentsOf(t, "c1")
	if len(a) != 1 || a[0].Kind != attach.KindUnread || !strings.Contains(a[0].Note, "password") {
		t.Errorf("attachments = %+v", a)
	}

	// with text: the user is told, and the model still answers (and knows why)
	rec = &recorder{}
	r.Handle(ctx, withFiles(rec, "what does it say?", file("secret.pdf", fixture(t, "encrypted.pdf"))))
	got := rec.all()
	if len(got) != 2 || !strings.Contains(got[0], "password") || got[1] != "pong" {
		t.Errorf("sent = %q", got)
	}
	if m := lastRequestUser(f); !strings.Contains(m.Content, "sent but not read") || !strings.Contains(m.Content, "password") {
		t.Errorf("the model was not told: %q", m.Content)
	}
}

func TestEachFailureHasAReason(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{MaxBytes: 1 << 20, PDFMaxPages: 5})
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"x.pdf", fixture(t, "corrupt.pdf"), "damaged"},
		{"x.zip", []byte("PK\x03\x04 not a document"), "isn't supported"},
		{"many.pdf", fixture(t, "many-pages.pdf"), "too many pages"},
		{"scan.pdf", fixture(t, "scanned-en.pdf"), "scan"},
		{"pic.png", fixture(t, "alpha.png"), "can't look at pictures"},
		{"huge.txt", make([]byte, 2<<20), "too large"},
	}
	for _, c := range cases {
		rec := &recorder{}
		r.Handle(ctx, withFiles(rec, "", file(c.name, c.data)))
		if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], c.want) || !strings.Contains(got[0], c.name) {
			t.Errorf("%s: sent = %q, want %q", c.name, got, c.want)
		}
	}
	if f.calls() != 0 {
		t.Error("the model was called")
	}
}

func TestOversizedFileIsNotDownloaded(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{MaxBytes: 1000})
	rec := &recorder{}
	big := file("big.txt", make([]byte, 5000))
	r.Handle(ctx, withFiles(rec, "", big))
	if big.fetches.Load() != 0 {
		t.Error("a file over the limit was downloaded")
	}
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "too large") {
		t.Errorf("sent = %q", got)
	}
}

func TestDownloadFailure(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	in := msg(rec, "")
	in.Attachments = []channel.Attachment{{Name: "a.txt", Fetch: func(context.Context, int64) ([]byte, error) { return nil, errors.New("telegram: 502") }}}
	r.Handle(ctx, in)
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "couldn't be downloaded") {
		t.Errorf("sent = %q", got)
	}
}

func TestFilesInUnaddressedGroupMessagesAreNotOpened(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	doc := file("minutes.pdf", fixture(t, "text-en.pdf"))
	in := withFiles(rec, "here are the minutes", doc)
	in.IsGroup, in.Addressed = true, false
	r.Handle(ctx, in)
	if doc.fetches.Load() != 0 || f.calls() != 0 || len(rec.all()) != 0 {
		t.Errorf("fetches=%d calls=%d sent=%q", doc.fetches.Load(), f.calls(), rec.all())
	}
	// a file with no text at all is kept too
	in = withFiles(rec, "", file("photo.png", fixture(t, "alpha.png")))
	in.IsGroup, in.Addressed = true, false
	r.Handle(ctx, in)
	a := r.attachmentsOf(t, "c1")
	if len(a) != 2 || a[0].Kind != attach.KindUnread || !strings.Contains(a[1].Note, "not addressed") {
		t.Errorf("attachments = %+v", a)
	}
	// the model learns of them when it is later addressed
	in = msg(rec, "what was in that PDF?")
	in.IsGroup = true
	r.Handle(ctx, in)
	f.mu.Lock()
	var all string
	for _, m := range f.requests[0].Messages {
		all += m.Content + "\n"
	}
	f.mu.Unlock()
	if !strings.Contains(all, `"minutes.pdf": a file was sent but not read`) {
		t.Errorf("the conversation does not mention the file: %q", all)
	}
}

func TestFileRateLimitAndCount(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{MaxPerMessage: 2, RatePerMinute: 3}, attach.Config{})
	rec := &recorder{}
	var files []fileSpec
	for i := 0; i < 4; i++ {
		files = append(files, file(fmt.Sprintf("n%d.txt", i), []byte(fmt.Sprintf("note number %d", i))))
	}
	r.Handle(ctx, withFiles(rec, "read these", files...))
	if got := rec.all(); len(got) != 2 || !strings.Contains(got[0], "at most 2 files") || got[1] != "pong" {
		t.Errorf("sent = %q", got)
	}
	if files[2].fetches.Load() != 0 || files[3].fetches.Load() != 0 {
		t.Error("files beyond the limit were downloaded")
	}
	// 2 used of 3 per minute; the next message may read only one more
	rec = &recorder{}
	more := []fileSpec{file("m0.txt", []byte("more zero")), file("m1.txt", []byte("more one"))}
	r.Handle(ctx, withFiles(rec, "and these", more...))
	if more[0].fetches.Load() != 1 || more[1].fetches.Load() != 0 {
		t.Errorf("fetches = %d, %d", more[0].fetches.Load(), more[1].fetches.Load())
	}
	if got := rec.all(); len(got) != 2 || !strings.Contains(got[0], "too quickly") {
		t.Errorf("sent = %q", got)
	}
}

func TestOldFilesAreEvicted(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{MaxChatBytes: 2000, RatePerMinute: 100}, attach.Config{})
	rec := &recorder{}
	for i := 0; i < 4; i++ {
		body := strings.Repeat(fmt.Sprintf("file %d ", i), 100) // ~700 bytes
		r.Handle(ctx, withFiles(rec, "x", file(fmt.Sprintf("f%d.txt", i), []byte(body))))
	}
	a := r.attachmentsOf(t, "c1")
	var total int64
	for _, x := range a {
		total += x.StoredBytes
	}
	if total > 2000 || len(a) == 0 || len(a) >= 4 {
		t.Errorf("%d attachments use %d bytes", len(a), total)
	}
	if a[0].Name != "f3.txt" {
		t.Errorf("the newest file must survive: %+v", a[0])
	}
}

func TestCommandsInACaptionAreNotRun(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	r.Handle(ctx, withFiles(rec, "/reset", file("a.txt", []byte("keep me"))))
	for _, s := range rec.all() {
		if strings.Contains(s, "cleared") {
			t.Error("a caption started with /reset and reset the chat")
		}
	}
	if f.calls() != 1 {
		t.Errorf("calls = %d", f.calls())
	}
}

func TestInboxCopies(t *testing.T) {
	f := &fakeLLM{}
	var mu sync.Mutex
	got := map[string][]byte{}
	var ws string
	r := attachRouter(t, f, agent.Config{Inbox: true}, AttachConfig{Inbox: func(_ context.Context, workspace, path string, data []byte) error {
		mu.Lock()
		defer mu.Unlock()
		ws, got[path] = workspace, data
		return nil
	}}, attach.Config{Vision: true})
	rec := &recorder{}
	pdf := fixture(t, "text-en.pdf")
	r.Handle(ctx, withFiles(rec, "x", file("My Report.pdf", pdf), file("p.png", fixture(t, "alpha.png"))))
	mu.Lock()
	defer mu.Unlock()
	if string(got["inbox/1-My_Report.pdf"]) != string(pdf) {
		t.Errorf("copies = %v", keys(got))
	}
	if len(got["inbox/2-p.jpg"]) == 0 || ws != sandbox.WorkspaceID("test:c1") {
		t.Errorf("copies = %v, workspace %q", keys(got), ws)
	}
}

func keys(m map[string][]byte) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	return k
}

func TestFilesWithoutSupportAreExplained(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{}) // attachments not enabled
	rec := &recorder{}
	r.Handle(ctx, withFiles(rec, "", file("a.txt", []byte("hello"))))
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "only read pictures") {
		t.Errorf("sent = %q", got)
	}
	if f.calls() != 0 {
		t.Error("the model was called")
	}
}

func TestResetDeletesFiles(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	r.Handle(ctx, withFiles(rec, "x", file("a.txt", []byte("remember"))))
	var dir string
	_ = r.sessions.Record("test", "c1", func(s *session.Session) error { dir, _ = s.FilesDir(); return nil })
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Fatal("no files stored")
	}
	r.Handle(ctx, msg(rec, "/reset"))
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("files left after /reset: %v", entries)
	}
	if a := r.attachmentsOf(t, "c1"); len(a) != 0 {
		t.Errorf("attachments left after /reset: %+v", a)
	}
}

func TestMessagesKeepTheirOrderWhileFilesAreRead(t *testing.T) {
	f := &fakeLLM{}
	r := attachRouter(t, f, agent.Config{}, AttachConfig{}, attach.Config{})
	rec := &recorder{}
	slow := make(chan struct{})
	started := make(chan struct{})
	in := msg(rec, "first, with a file")
	in.Attachments = []channel.Attachment{{Name: "a.txt", Fetch: func(context.Context, int64) ([]byte, error) {
		close(started)
		<-slow
		return []byte("late file"), nil
	}}}
	accepted := make(chan struct{})
	in.Accepted = func() { close(accepted) }
	done := make(chan struct{})
	go func() { r.Handle(ctx, in); close(done) }()
	<-accepted // stored; the download has not finished
	<-started
	second := msg(rec, "second")
	secondAccepted := make(chan struct{})
	second.Accepted = func() { close(secondAccepted) }
	done2 := make(chan struct{})
	go func() { r.Handle(ctx, second); close(done2) }()
	<-secondAccepted // stored while the first message's file is still downloading
	close(slow)
	<-done
	<-done2
	_ = r.sessions.Record("test", "c1", func(s *session.Session) error {
		st, _ := s.Messages(ctx)
		var users []string
		for _, m := range st {
			if m.Message.Role == llm.RoleUser {
				users = append(users, m.Message.Content)
			}
		}
		if strings.Join(users, "|") != "first, with a file|second" {
			t.Errorf("order = %q", users)
		}
		return nil
	})
}
