package attach

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// PDFEngine opens PDF documents. Implementations: the sandbox (poppler and
// Tesseract, isolated from the bot's secrets) and a pure-Go reader.
type PDFEngine interface {
	Name() string
	// Open parses data. workspace is the sandbox workspace for scratch files.
	// It returns ErrEncrypted or ErrCorrupt for such files; any other error
	// means the engine is unavailable and the next one is tried.
	Open(ctx context.Context, workspace string, data []byte) (PDFDoc, error)
}

// PDFDoc is an opened document.
type PDFDoc interface {
	Pages() int
	// Text returns the text of each page (up to maxPages pages).
	Text(ctx context.Context, maxPages int) ([]string, error)
	// Render draws a page (1-based) as a JPEG whose longer side is maxEdge.
	// Engines that cannot return ErrUnsupported.
	Render(ctx context.Context, page, maxEdge int) ([]byte, error)
	// OCR recognises the text of pages (1-based) in the given Tesseract
	// languages. Engines that cannot return ErrUnsupported.
	OCR(ctx context.Context, pages []int, langs string) (map[int]string, error)
	Close()
}

func (p *Processor) open(ctx context.Context, workspace string, data []byte) (PDFDoc, string, error) {
	var last error
	for _, e := range p.engines {
		doc, err := e.Open(ctx, workspace, data)
		if err == nil {
			return doc, e.Name(), nil
		}
		if errors.Is(err, ErrEncrypted) || errors.Is(err, ErrCorrupt) || ctx.Err() != nil {
			return nil, "", err
		}
		p.log.Warn("pdf engine unavailable, trying the next", "engine", e.Name(), "err", err)
		last = err
	}
	if last == nil {
		last = ErrUnsupported
	}
	return nil, "", last
}

func (p *Processor) pdf(ctx context.Context, workspace string, res *Result, data []byte) error {
	doc, engine, err := p.open(ctx, workspace, data)
	if err != nil {
		return err
	}
	defer doc.Close()
	if n := doc.Pages(); n > p.cfg.PDFMaxPages {
		return ErrTooManyPages
	} else if n < 1 {
		return ErrCorrupt
	}
	pages, err := doc.Text(ctx, p.cfg.PDFMaxPages)
	if err != nil {
		return err
	}
	for i := range pages {
		pages[i] = cleanText(pages[i])
	}

	var blank []int // pages with no text layer
	for i, t := range pages {
		if nonBlank(t) < p.cfg.ScanMinChars {
			blank = append(blank, i+1)
		}
	}
	scanned := len(blank)*2 >= len(pages) // at least half the document has no text
	if scanned {
		p.readScan(ctx, doc, res, pages, blank)
	}
	text := strings.Join(pages, PageSep)
	p.finishText(res, text)
	if scanned && nonBlank(text) == 0 && len(res.Images) == 0 {
		return ErrScanned
	}
	res.Pages = len(pages)
	res.Original = data
	p.log.Debug("pdf processed", "engine", engine, "pages", res.Pages, "chars", res.Chars, "scanned", scanned)
	return nil
}

// readScan handles a document that is mostly pictures of pages: it
// recognises the text of the blank pages with OCR, and (for a vision model)
// renders the first few of them as pictures.
func (p *Processor) readScan(ctx context.Context, doc PDFDoc, res *Result, pages []string, blank []int) {
	if p.cfg.OCRLangs != "" {
		todo := blank
		if len(todo) > p.cfg.OCRMaxPages {
			todo = todo[:p.cfg.OCRMaxPages]
		}
		got, err := doc.OCR(ctx, todo, p.cfg.OCRLangs)
		switch {
		case err == nil || len(got) > 0:
			for pg, t := range got {
				pages[pg-1] = cleanOCR(cleanText(t))
			}
			res.Note = joinNote(res.Note, "scanned document, text recognised with OCR ("+p.cfg.OCRLangs+")")
			if len(blank) > len(todo) {
				res.Note = joinNote(res.Note, "only the first pages were recognised")
			}
		case errors.Is(err, ErrUnsupported):
			// no OCR on this engine
		default:
			p.log.Warn("pdf ocr failed", "err", err)
		}
	}
	if p.cfg.Vision {
		for _, pg := range blank {
			if len(res.Images) >= p.cfg.PageImages {
				break
			}
			jpg, err := doc.Render(ctx, pg, p.cfg.ImageMaxEdge)
			if errors.Is(err, ErrUnsupported) {
				break
			}
			if err != nil {
				p.log.Warn("pdf render failed", "page", pg, "err", err)
				break
			}
			if img, err := prepareImage(jpg, p.cfg.ImageMaxEdge, p.cfg.MaxImagePixels); err == nil {
				res.Images = append(res.Images, img.JPEG)
			}
		}
		if len(res.Images) > 0 {
			res.Note = joinNote(res.Note, "pictures of the first scanned pages are attached")
		}
	}
}

func nonBlank(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

var thaiGap = regexp.MustCompile(`([\x{0E01}-\x{0E5B}]) ([\x{0E01}-\x{0E5B}])`)

// cleanOCR repairs Tesseract's habit of putting a space between every Thai
// letter: on lines where most neighbouring Thai letters are separated, the
// spaces between Thai letters are removed.
func cleanOCR(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		gaps := len(thaiGap.FindAllStringIndex(l, -1))
		thai := 0
		for _, r := range l {
			if r >= 0x0E01 && r <= 0x0E5B {
				thai++
			}
		}
		if gaps >= 2 && gaps*10 >= thai*4 {
			for thaiGap.MatchString(l) {
				l = thaiGap.ReplaceAllString(l, "$1$2")
			}
			lines[i] = l
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
