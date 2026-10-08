// Package attach turns files that users send (pictures, PDFs, text files)
// into something a language model can read: shrunk JPEGs for vision, and
// extracted text split into pages.
//
// Untrusted files are parsed, where possible, inside the sandbox rather than
// in the bot process, which holds the channel tokens and API keys. The
// pure-Go fallback keeps the feature working without a sandbox.
package attach

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Errors a caller may want to explain to the user.
var (
	ErrCorrupt      = errors.New("attach: the file is damaged or not what it claims to be")
	ErrImageTooBig  = errors.New("attach: the picture has too many pixels")
	ErrTooLarge     = errors.New("attach: the file is too large")
	ErrUnsupported  = errors.New("attach: this kind of file is not supported")
	ErrNotText      = errors.New("attach: the file is named like a text file but holds binary data or an unknown encoding")
	ErrEncrypted    = errors.New("attach: the document is password protected")
	ErrTooManyPages = errors.New("attach: the document has too many pages")
	ErrVisionOff    = errors.New("attach: the model cannot look at pictures")
	ErrScanned      = errors.New("attach: the document is a scan and cannot be read without OCR or a vision model")
)

// PageSep separates pages in Result.Text.
const PageSep = "\f"

// Config bounds what is accepted and decides how documents are presented.
// Zero values select the defaults.
type Config struct {
	// MaxBytes is the largest file accepted (default 20 MiB).
	MaxBytes int64
	// Vision says whether the model can look at pictures.
	Vision bool
	// ImageMaxEdge is the longest side, in pixels, of pictures sent to the
	// model (default 1568); MaxImagePixels refuses larger pictures before
	// decoding them (default 40 million).
	ImageMaxEdge   int
	MaxImagePixels int64
	// PDFMaxPages is the most pages of a document that are read (default 200);
	// longer documents are refused.
	PDFMaxPages int
	// MaxTextChars caps the text kept per file (default 1,000,000).
	MaxTextChars int
	// InlineChars is the longest text shown to the model together with the
	// message; longer text is read on demand (default 6000).
	InlineChars int
	// PageChars is the size of a "page" of a plain text file (default 6000).
	PageChars int
	// OCRLangs are Tesseract languages, e.g. "eng+tha". Empty disables OCR.
	OCRLangs string
	// OCRMaxPages is the most pages recognised per document (default 15).
	OCRMaxPages int
	// PageImages is how many pages of a scanned document are shown to a
	// vision model as pictures (default 3; negative for none).
	PageImages int
	// ScanMinChars is the number of non-blank characters below which a page
	// counts as having no text (default 25).
	ScanMinChars int
	// Timeout bounds the processing of one file (default 90 s).
	Timeout time.Duration
	// Concurrency is how many files are processed at once (default 2).
	Concurrency int
}

func (c Config) withDefaults() Config {
	setI := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 20 << 20
	}
	if c.MaxImagePixels <= 0 {
		c.MaxImagePixels = 40_000_000
	}
	setI(&c.ImageMaxEdge, 1568)
	setI(&c.PDFMaxPages, 200)
	setI(&c.MaxTextChars, 1_000_000)
	setI(&c.InlineChars, 6000)
	setI(&c.PageChars, 6000)
	setI(&c.OCRMaxPages, 15)
	switch {
	case c.PageImages == 0:
		c.PageImages = 3
	case c.PageImages < 0: // explicitly none
		c.PageImages = 0
	}
	setI(&c.ScanMinChars, 25)
	setI(&c.Concurrency, 2)
	if c.Timeout <= 0 {
		c.Timeout = 90 * time.Second
	}
	if !validLangs(c.OCRLangs) {
		c.OCRLangs = ""
	}
	return c
}

// validLangs accepts Tesseract language lists such as "eng+tha". The value
// ends up in a shell command, so anything else is rejected.
func validLangs(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '+') {
			return false
		}
	}
	return true
}

// Input is a file as received from a chat channel.
type Input struct {
	Name string // as sent; untrusted
	MIME string // as declared; untrusted
	Data []byte
}

// Result is a processed attachment.
type Result struct {
	Kind string // KindImage, KindPDF or KindText
	Name string // cleaned for display
	MIME string // as detected
	// Original is the file to keep for documents; nil for pictures, where
	// only the re-encoded Images are kept.
	Original []byte
	// Images are JPEGs for the model: the picture, or pages of a scan.
	Images [][]byte
	// Text is the extracted text, pages separated by PageSep.
	Text   string
	Pages  int
	Chars  int
	Inline bool
	// Note describes how the text was obtained when that is worth telling the
	// model (OCR, truncation, ...).
	Note string
}

// Processor converts files. It is safe for concurrent use.
type Processor struct {
	cfg     Config
	engines []PDFEngine
	log     *slog.Logger
	slots   chan struct{}
}

// New returns a Processor. PDFs are opened with the first engine that can
// handle them, so the sandboxed engine goes first and the in-process one last.
func New(cfg Config, log *slog.Logger, engines ...PDFEngine) *Processor {
	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	var es []PDFEngine
	for _, e := range engines {
		if e != nil {
			es = append(es, e)
		}
	}
	return &Processor{cfg: cfg, engines: es, log: log, slots: make(chan struct{}, cfg.Concurrency)}
}

// Config returns the effective configuration.
func (p *Processor) Config() Config { return p.cfg }

// Process analyses one file. workspace names the sandbox workspace used for
// temporary files while a document is parsed.
func (p *Processor) Process(ctx context.Context, workspace string, in Input) (*Result, error) {
	if int64(len(in.Data)) > p.cfg.MaxBytes {
		return nil, ErrTooLarge
	}
	if len(in.Data) == 0 {
		return nil, ErrCorrupt
	}
	kind, mime := sniff(in.Name, in.MIME, in.Data)
	if kind == "" {
		if claimsText(in.Name, in.MIME) {
			return nil, ErrNotText
		}
		return nil, ErrUnsupported
	}
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	res := &Result{Kind: kind, Name: cleanName(in.Name), MIME: mime}
	var err error
	switch kind {
	case KindImage:
		err = p.image(res, in.Data)
	case KindText:
		err = p.text(res, in.Data)
	case KindPDF:
		err = p.pdf(ctx, workspace, res, in.Data)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *Processor) image(res *Result, data []byte) error {
	if !p.cfg.Vision {
		return ErrVisionOff
	}
	img, err := prepareImage(data, p.cfg.ImageMaxEdge, p.cfg.MaxImagePixels)
	if err != nil {
		return err
	}
	res.Images = [][]byte{img.JPEG}
	res.MIME = "image/jpeg"
	return nil
}

// cleanName reduces a sender-supplied file name to a short, printable base
// name. It is shown to the model and the user, never used as a path.
func cleanName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		if unicode.IsControl(r) || r == utf8.RuneError || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimSpace(b.String())
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	if r := []rune(name); len(r) > 80 {
		ext := filepath.Ext(name)
		if len([]rune(ext)) > 10 {
			ext = ""
		}
		name = string(r[:80-len([]rune(ext))]) + ext
	}
	return name
}

// finishText applies the character cap, counts, and decides on inlining. The
// text must already use PageSep between pages.
func (p *Processor) finishText(res *Result, text string) {
	if r := []rune(text); len(r) > p.cfg.MaxTextChars {
		text = string(r[:p.cfg.MaxTextChars])
		res.Note = joinNote(res.Note, "text truncated")
	}
	res.Text = text
	res.Pages = strings.Count(text, PageSep) + 1
	res.Chars = utf8.RuneCountInString(strings.ReplaceAll(text, PageSep, ""))
	res.Inline = nonBlank(text) > 0 && res.Chars <= p.cfg.InlineChars
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// KindUnread marks an attachment that was received but not opened, with the
// reason in its note.
const KindUnread = "unread"

// InboxPath is where a copy of an attachment is placed in the chat's command
// workspace, relative to it. The name is reduced to plain ASCII so that it
// needs no quoting in a shell; pictures are copies of the JPEG sent to the model.
func InboxPath(id int64, name, kind string) string {
	ext := strings.ToLower(filepath.Ext(name))
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if kind == KindImage {
		ext = ".jpg"
	}
	clean := func(s string, max int) string {
		var b strings.Builder
		under := false
		for _, r := range s {
			if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.') {
				b.WriteRune(r)
				under = false
			} else if !under {
				b.WriteByte('_')
				under = true
			}
		}
		out := strings.Trim(b.String(), "._")
		if len(out) > max {
			out = out[:max]
		}
		return out
	}
	ext = "." + clean(strings.TrimPrefix(ext, "."), 8)
	if ext == "." {
		ext = ""
	}
	base = clean(base, 40)
	if base == "" {
		base = "file"
	}
	return fmt.Sprintf("inbox/%d-%s%s", id, base, ext)
}

// CleanName reduces a sender-supplied file name to a short, printable base
// name, for showing and for building names of our own.
func CleanName(name string) string { return cleanName(name) }
