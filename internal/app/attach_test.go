package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/attach"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/sandbox"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func fakeOllama(t *testing.T, body string) *llm.Ollama {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
	t.Cleanup(srv.Close)
	return &llm.Ollama{BaseURL: srv.URL, Client: srv.Client()}
}

func TestVisionDetection(t *testing.T) {
	vision := fakeOllama(t, `{"capabilities":["completion","vision"]}`)
	text := fakeOllama(t, `{"capabilities":["completion"]}`)
	cases := []struct {
		mode string
		p    llm.Provider
		want bool
	}{
		{"auto", vision, true}, {"auto", text, false}, {"on", text, true}, {"off", vision, false},
		{"auto", &llm.OpenAI{}, true}, {"off", &llm.OpenAI{}, false},
	}
	for _, c := range cases {
		cfg := &config.Config{Vision: c.mode, LLMModel: "m"}
		if got := detectVision(context.Background(), cfg, c.p, quiet()); got != c.want {
			t.Errorf("vision=%s with %T: got %v, want %v", c.mode, c.p, got, c.want)
		}
	}
}

func TestVisionDetectionGivesUpWhenOllamaIsDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if detectVision(ctx, &config.Config{Vision: "auto"}, &llm.Ollama{BaseURL: url, Client: http.DefaultClient}, quiet()) {
		t.Error("pictures were switched on without knowing that the model can see")
	}
}

// fakeBackend is a sandbox that answers /info and nothing else.
type fakeBackend struct {
	info  sandbox.Info
	files map[string][]byte
}

func (f *fakeBackend) Run(context.Context, sandbox.Request) (*sandbox.Result, error) {
	return &sandbox.Result{}, nil
}
func (f *fakeBackend) Reset(context.Context, string) error { return nil }
func (f *fakeBackend) Info() sandbox.Info                  { return f.info }
func (f *fakeBackend) Put(_ context.Context, ws, p string, r io.Reader, _ int64) error {
	b, _ := io.ReadAll(r)
	f.files[ws+"/"+p] = b
	return nil
}
func (f *fakeBackend) Get(context.Context, string, string, int64) ([]byte, error) {
	return nil, sandbox.ErrNotFound
}
func (f *fakeBackend) Remove(context.Context, string, string) error { return nil }

const token = "0123456789abcdef0123456789abcdef"

func startSandbox(t *testing.T, b *fakeBackend) string {
	srv, err := sandbox.NewServer(b, token, time.Minute, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

func attachConfig(url string) *config.Config {
	return &config.Config{
		Attachments: true, Vision: "on", PDFEngine: "auto", OCRLangs: "auto", Lang: "en", SandboxURL: url, SandboxToken: token,
		AttachMaxMB: 20, AttachPerMessage: 5, AttachRate: 10, AttachChatMB: 100, AttachInlineChars: 6000, ImageMaxEdge: 1568,
		PDFMaxPages: 200, OCRMaxPages: 15, VisionPages: 3, RunCommand: "off", AttachInbox: true,
	}
}

func TestPDFsGoToTheSandboxWhenItCanReadThem(t *testing.T) {
	ready := &fakeBackend{files: map[string][]byte{}, info: sandbox.Info{Files: true, Tools: []string{"pdfinfo", "pdftotext", "pdftoppm", "tesseract"}}}
	cfg := attachConfig(startSandbox(t, ready))
	engines, ocr, render := pdfEngines(context.Background(), cfg, nil, quiet())
	if len(engines) != 2 || engines[0].Name() != "sandbox" || engines[1].Name() != "native" || !ocr || !render {
		t.Errorf("engines = %v ocr=%v render=%v", names(engines), ocr, render)
	}
	// the sandbox is asked first: a PDF reaches it
	p := attach.New(attach.Config{}, quiet(), engines...)
	_, _ = p.Process(context.Background(), "ws", attach.Input{Name: "a.pdf", Data: []byte("%PDF-1.4\n")})
	var uploaded bool
	for k := range ready.files {
		if strings.HasSuffix(k, "/doc.pdf") {
			uploaded = true
		}
	}
	if !uploaded {
		t.Error("the PDF was not uploaded to the sandbox")
	}

	// no poppler in the sandbox: the built-in reader, no OCR
	bare := &fakeBackend{files: map[string][]byte{}, info: sandbox.Info{Files: true, Tools: []string{"sh"}}}
	engines, ocr, render = pdfEngines(context.Background(), attachConfig(startSandbox(t, bare)), nil, quiet())
	if len(engines) != 1 || engines[0].Name() != "native" || ocr || render {
		t.Errorf("without poppler: %v ocr=%v render=%v", names(engines), ocr, render)
	}
	// an old sandbox without file transfer
	old := &fakeBackend{files: map[string][]byte{}, info: sandbox.Info{Tools: []string{"pdfinfo", "pdftotext"}}}
	engines, _, _ = pdfEngines(context.Background(), attachConfig(startSandbox(t, old)), nil, quiet())
	if len(engines) != 1 || engines[0].Name() != "native" {
		t.Errorf("without file transfer: %v", names(engines))
	}
	// the user can insist on the built-in reader, or have no sandbox at all
	cfg = attachConfig(startSandbox(t, ready))
	cfg.PDFEngine = "native"
	if engines, _, _ = pdfEngines(context.Background(), cfg, nil, quiet()); len(engines) != 1 || engines[0].Name() != "native" {
		t.Errorf("native: %v", names(engines))
	}
	if engines, _, _ = pdfEngines(context.Background(), attachConfig(""), nil, quiet()); len(engines) != 1 || engines[0].Name() != "native" {
		t.Errorf("no sandbox: %v", names(engines))
	}
	// pdf-engine=sandbox never falls back to reading in the bot's process when the sandbox is usable
	cfg = attachConfig(startSandbox(t, ready))
	cfg.PDFEngine = "sandbox"
	if engines, _, _ = pdfEngines(context.Background(), cfg, nil, quiet()); len(engines) != 1 || engines[0].Name() != "sandbox" {
		t.Errorf("sandbox only: %v", names(engines))
	}
}

func names(es []attach.PDFEngine) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestAttachmentSettings(t *testing.T) {
	ready := &fakeBackend{files: map[string][]byte{}, info: sandbox.Info{Files: true, Tools: []string{"pdfinfo", "pdftotext"}}}
	cfg := attachConfig(startSandbox(t, ready))
	cfg.Lang = "th"
	a := newAttachments(context.Background(), cfg, &llm.OpenAI{}, nil, quiet())
	pc := a.processor.Config()
	// Tesseract is missing in this sandbox, so OCR is off whatever was asked
	if !a.vision || pc.OCRLangs != "" || pc.MaxBytes != 20<<20 || pc.ImageMaxEdge != 1568 {
		t.Errorf("vision=%v config=%+v", a.vision, pc)
	}
	// no pdftoppm: pages cannot be shown as pictures
	if pc.PageImages != 0 {
		t.Errorf("PageImages = %d", pc.PageImages)
	}
	cfg.Attachments = false
	if newAttachments(context.Background(), cfg, &llm.OpenAI{}, nil, quiet()) != nil {
		t.Error("attachments are off but a setup was built")
	}
}

type recordingRunner struct {
	sandbox.Runner
	got map[string][]byte
}

func (r *recordingRunner) Put(_ context.Context, ws, p string, rd io.Reader, max int64) error {
	b, _ := io.ReadAll(rd)
	if int64(len(b)) > max {
		return sandbox.ErrTooLarge
	}
	r.got[ws+"|"+p] = b
	return nil
}
func (r *recordingRunner) Get(context.Context, string, string, int64) ([]byte, error) {
	return nil, nil
}
func (r *recordingRunner) Remove(context.Context, string, string) error { return nil }

func TestInboxCopyGoesThroughTheRunner(t *testing.T) {
	rr := &recordingRunner{got: map[string][]byte{}}
	cfg := attachConfig("")
	a := newAttachments(context.Background(), cfg, &llm.OpenAI{}, &commandRunner{runner: rr}, quiet())
	rc := a.routerConfig(cfg)
	if rc.Inbox == nil {
		t.Fatal("no inbox")
	}
	data := bytes.Repeat([]byte("x"), 100)
	if err := rc.Inbox(context.Background(), "ws1", "inbox/1-a.txt", data); err != nil || len(rr.got["ws1|inbox/1-a.txt"]) != 100 {
		t.Errorf("%v %v", err, rr.got)
	}
	cfg.AttachInbox = false
	if newAttachments(context.Background(), cfg, &llm.OpenAI{}, &commandRunner{runner: rr}, quiet()).routerConfig(cfg).Inbox != nil {
		t.Error("inbox copies were not switched off")
	}
	cfg.AttachInbox, cfg.AttachRate = true, 0
	if rate := a.routerConfig(cfg).RatePerMinute; rate < 1000 {
		t.Errorf("--attach-rate=0 must mean unlimited, got %d", rate)
	}
}
