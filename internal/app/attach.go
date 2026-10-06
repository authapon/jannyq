package app

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/authapon/jannyq/internal/attach"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/router"
	"github.com/authapon/jannyq/internal/sandbox"
)

// attachments is the file-reading setup of a running bot.
type attachments struct {
	processor *attach.Processor
	vision    bool
	// files copies a file into a chat's command workspace; nil when that is off.
	files sandbox.Files
}

// detectVision decides whether the model can look at pictures.
func detectVision(ctx context.Context, cfg *config.Config, provider llm.Provider, log *slog.Logger) bool {
	switch cfg.Vision {
	case "on":
		return true
	case "off":
		return false
	}
	o, ok := provider.(*llm.Ollama)
	if !ok {
		// An OpenAI-compatible API cannot be asked. Current models mostly accept
		// pictures; --vision=off turns it off for one that does not.
		log.Info("assuming that the model accepts pictures (set --vision=off if it does not)", "model", cfg.LLMModel)
		return true
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ok, err := o.SupportsVision(ctx, cfg.LLMModel)
		if err == nil {
			log.Info("vision support detected", "model", cfg.LLMModel, "vision", ok)
			return ok
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			log.Warn("could not ask Ollama whether the model can see pictures; pictures are off (set --vision=on to force)", "err", err)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// pdfEngines chooses where PDFs are read. The sandbox comes first: a PDF is
// untrusted input to a large parser, and the sandbox holds no secrets. The
// in-process reader is the fallback (and the only choice without a sandbox).
func pdfEngines(ctx context.Context, cfg *config.Config, cr *commandRunner, log *slog.Logger) (engines []attach.PDFEngine, ocr, render bool) {
	var runner sandbox.Runner
	var files sandbox.Files
	var info *sandbox.Info
	switch {
	case cfg.PDFEngine == "native":
	case cr != nil && cr.runner != nil:
		runner, info = cr.runner, cr.info
		files, _ = cr.runner.(sandbox.Files)
	case cfg.SandboxURL != "" && len(cfg.SandboxToken) >= 16:
		client := &sandbox.Client{BaseURL: cfg.SandboxURL, Token: cfg.SandboxToken, HTTP: &http.Client{}}
		runner, files = client, client
		info = waitForInfo(ctx, client, log)
	}
	if runner != nil && files != nil {
		has := func(t string) bool { return info != nil && slices.Contains(info.Tools, t) }
		if info != nil && info.Files && has("pdfinfo") && has("pdftotext") {
			engines = append(engines, &attach.SandboxEngine{Runner: runner, Files: files})
			ocr, render = has("tesseract") && has("pdftoppm"), has("pdftoppm")
			log.Info("PDFs are read in the sandbox", "ocr", ocr, "page_pictures", render)
		} else {
			log.Warn("the sandbox cannot read PDFs (it needs poppler-utils, tesseract-ocr and file transfer); using the built-in reader without OCR")
		}
	}
	if cfg.PDFEngine != "sandbox" || len(engines) == 0 {
		if len(engines) == 0 {
			log.Info("PDFs are read inside the bot process, without OCR; point --sandbox-url at the sandbox to read them there")
		}
		engines = append(engines, attach.NativeEngine{})
	}
	return engines, ocr, render
}

// newAttachments builds the file-reading setup, or returns nil when it is off.
func newAttachments(ctx context.Context, cfg *config.Config, provider llm.Provider, cr *commandRunner, log *slog.Logger) *attachments {
	if !cfg.Attachments {
		return nil
	}
	a := &attachments{vision: detectVision(ctx, cfg, provider, log)}
	engines, ocr, render := pdfEngines(ctx, cfg, cr, log)
	langs := cfg.OCRLanguages()
	if !ocr {
		langs = ""
	}
	pages := cfg.VisionPages
	if !render {
		pages = 0
	}
	if pages == 0 {
		pages = -1 // Config treats 0 as "default"; negative values mean none
	}
	a.processor = attach.New(attach.Config{
		MaxBytes:     int64(cfg.AttachMaxMB) << 20,
		Vision:       a.vision,
		ImageMaxEdge: cfg.ImageMaxEdge,
		PDFMaxPages:  cfg.PDFMaxPages,
		InlineChars:  cfg.AttachInlineChars,
		OCRLangs:     langs,
		OCRMaxPages:  cfg.OCRMaxPages,
		PageImages:   pages,
		Timeout:      2 * time.Minute,
	}, log, engines...)
	if cfg.AttachInbox && cr != nil && cr.runner != nil {
		a.files, _ = cr.runner.(sandbox.Files)
	}
	log.Info("attachments enabled", "vision", a.vision, "max_mb", cfg.AttachMaxMB, "ocr", langs, "inbox", a.files != nil)
	return a
}

// routerConfig converts the settings for the router.
func (a *attachments) routerConfig(cfg *config.Config) router.AttachConfig {
	c := router.AttachConfig{
		Processor:     a.processor,
		MaxPerMessage: cfg.AttachPerMessage,
		RatePerMinute: cfg.AttachRate,
		MaxChatBytes:  int64(cfg.AttachChatMB) << 20,
	}
	if cfg.AttachRate == 0 {
		c.RatePerMinute = 1 << 20
	}
	if a.files != nil {
		files := a.files
		c.Inbox = func(ctx context.Context, workspace, path string, data []byte) error {
			return files.Put(ctx, workspace, path, bytes.NewReader(data), int64(len(data))+1)
		}
	}
	return c
}
