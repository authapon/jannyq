package attach

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/authapon/jannyq/internal/sandbox"
)

// SandboxEngine parses PDFs with poppler-utils and Tesseract inside the
// sandbox: the file is uploaded to the chat's workspace, processed under the
// sandbox's resource limits and deleted again. A malicious PDF can therefore
// do no more than any command the model could run.
type SandboxEngine struct {
	Runner sandbox.Runner
	Files  sandbox.Files
	// StepTimeout bounds each command (default 60 s).
	StepTimeout time.Duration
	// MaxOutput bounds a file read back from the sandbox (default 16 MiB).
	MaxOutput int64
}

func (e *SandboxEngine) Name() string { return "sandbox" }

func (e *SandboxEngine) timeout() int {
	d := e.StepTimeout
	if d <= 0 {
		d = 60 * time.Second
	}
	return max(1, int(d/time.Second))
}

func (e *SandboxEngine) maxOutput() int64 {
	if e.MaxOutput > 0 {
		return e.MaxOutput
	}
	return 16 << 20
}

var pagesRe = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)

func (e *SandboxEngine) Open(ctx context.Context, workspace string, data []byte) (PDFDoc, error) {
	if e.Runner == nil || e.Files == nil {
		return nil, errors.New("attach: sandbox engine is not configured")
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	d := &sandboxDoc{e: e, ws: workspace, dir: ".attach/" + hex.EncodeToString(id[:])}
	if err := e.Files.Put(ctx, workspace, d.dir+"/doc.pdf", bytes.NewReader(data), int64(len(data))+1); err != nil {
		return nil, fmt.Errorf("attach: uploading to the sandbox: %w", err)
	}
	out, code, err := d.run(ctx, "pdfinfo "+d.dir+"/doc.pdf")
	if err != nil {
		d.Close()
		return nil, err
	}
	if code != 0 {
		d.Close()
		low := strings.ToLower(out)
		switch {
		case strings.Contains(low, "incorrect password"):
			return nil, ErrEncrypted
		case code == 127 || strings.Contains(low, "not found"):
			return nil, errors.New("attach: poppler-utils is not installed in the sandbox")
		default:
			return nil, ErrCorrupt
		}
	}
	m := pagesRe.FindStringSubmatch(out)
	if m == nil {
		d.Close()
		return nil, ErrCorrupt
	}
	d.pages, _ = strconv.Atoi(m[1])
	return d, nil
}

type sandboxDoc struct {
	e     *SandboxEngine
	ws    string
	dir   string
	pages int
}

func (d *sandboxDoc) Pages() int { return d.pages }

// Close deletes the scratch directory. It uses its own context so that the
// files are removed even when the request was cancelled.
func (d *sandboxDoc) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, _ = d.run(ctx, "rm -rf "+d.dir)
}

func (d *sandboxDoc) run(ctx context.Context, cmd string) (string, int, error) {
	res, err := d.e.Runner.Run(ctx, sandbox.Request{
		Workspace: d.ws, Command: cmd, TimeoutSeconds: d.e.timeout(), MaxOutputBytes: 8 << 10,
	})
	if err != nil {
		return "", 0, err
	}
	if res.TimedOut {
		return res.Output, res.ExitCode, errors.New("attach: the sandbox command timed out")
	}
	if res.WritesDisabled {
		return res.Output, res.ExitCode, errors.New("attach: the sandbox workspace is full")
	}
	return res.Output, res.ExitCode, nil
}

func (d *sandboxDoc) Text(ctx context.Context, maxPages int) ([]string, error) {
	n := min(d.pages, maxPages)
	out, code, err := d.run(ctx, fmt.Sprintf("pdftotext -f 1 -l %d -enc UTF-8 %s/doc.pdf %s/out.txt", n, d.dir, d.dir))
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("attach: pdftotext failed: %s", firstLine(out))
	}
	raw, err := d.e.Files.Get(ctx, d.ws, d.dir+"/out.txt", d.e.maxOutput())
	if err != nil {
		return nil, fmt.Errorf("attach: reading the text from the sandbox: %w", err)
	}
	pages := strings.Split(string(raw), "\f")
	if len(pages) > n && strings.TrimSpace(pages[len(pages)-1]) == "" {
		pages = pages[:len(pages)-1]
	}
	for len(pages) < n {
		pages = append(pages, "")
	}
	return pages[:n], nil
}

func (d *sandboxDoc) Render(ctx context.Context, page, maxEdge int) ([]byte, error) {
	if page < 1 || page > d.pages {
		return nil, ErrCorrupt
	}
	cmd := fmt.Sprintf("rm -f %[1]s/render.jpg && pdftoppm -jpeg -jpegopt quality=88 -scale-to %[3]d -f %[2]d -l %[2]d %[1]s/doc.pdf %[1]s/r && mv %[1]s/r-*.jpg %[1]s/render.jpg",
		d.dir, page, maxEdge)
	out, code, err := d.run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if code == 127 {
		return nil, ErrUnsupported
	}
	if code != 0 {
		return nil, fmt.Errorf("attach: pdftoppm failed: %s", firstLine(out))
	}
	return d.e.Files.Get(ctx, d.ws, d.dir+"/render.jpg", d.e.maxOutput())
}

func (d *sandboxDoc) OCR(ctx context.Context, pages []int, langs string) (map[int]string, error) {
	if !validLangs(langs) {
		return nil, ErrUnsupported
	}
	got := map[int]string{}
	for _, pg := range pages {
		if pg < 1 || pg > d.pages {
			continue
		}
		o := d.dir + "/ocr"
		cmd := fmt.Sprintf("rm -rf %[1]s && mkdir %[1]s && pdftoppm -png -gray -scale-to 2400 -f %[2]d -l %[2]d %[3]s/doc.pdf %[1]s/p && OMP_THREAD_LIMIT=1 tesseract %[1]s/p-*.png %[1]s/out -l %[4]s",
			o, pg, d.dir, langs)
		out, code, err := d.run(ctx, cmd)
		if err != nil {
			return got, err
		}
		if code == 127 || strings.Contains(strings.ToLower(out), "tesseract: not found") {
			return got, ErrUnsupported
		}
		if code != 0 {
			return got, fmt.Errorf("attach: tesseract failed: %s", firstLine(out))
		}
		raw, err := d.e.Files.Get(ctx, d.ws, o+"/out.txt", d.e.maxOutput())
		if err != nil {
			return got, err
		}
		got[pg] = string(raw)
	}
	return got, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
