package attach

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/ledongthuc/pdf"
)

// NativeEngine reads the text layer of PDFs in the bot's own process. It is
// the fallback when there is no sandbox: it cannot render pages or run OCR,
// and a hostile file is parsed next to the bot's secrets, so prefer the
// sandboxed engine.
type NativeEngine struct{}

func (NativeEngine) Name() string { return "native" }

func (NativeEngine) Open(ctx context.Context, _ string, data []byte) (doc PDFDoc, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, ErrCorrupt
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		if errors.Is(err, pdf.ErrInvalidPassword) {
			return nil, ErrEncrypted
		}
		return nil, ErrCorrupt
	}
	return &nativeDoc{r: r}, nil
}

type nativeDoc struct{ r *pdf.Reader }

func (d *nativeDoc) Pages() int { return d.r.NumPage() }
func (d *nativeDoc) Close()     {}

func (d *nativeDoc) Text(ctx context.Context, maxPages int) ([]string, error) {
	n := min(d.r.NumPage(), maxPages)
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t, err := d.pageText(i)
		if err != nil {
			t = "" // an unreadable page is treated as having no text
		}
		out = append(out, t)
	}
	return out, nil
}

func (d *nativeDoc) pageText(i int) (t string, err error) {
	defer func() {
		if r := recover(); r != nil {
			t, err = "", fmt.Errorf("pdf page %d: %v", i, r)
		}
	}()
	p := d.r.Page(i)
	if p.V.IsNull() {
		return "", nil
	}
	return p.GetPlainText(nil)
}

func (*nativeDoc) Render(context.Context, int, int) ([]byte, error) { return nil, ErrUnsupported }
func (*nativeDoc) OCR(context.Context, []int, string) (map[int]string, error) {
	return nil, ErrUnsupported
}
