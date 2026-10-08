package attach

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/sandbox"
)

var bg = context.Background()

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// localBox runs commands in a temporary directory without any isolation. It
// stands in for the sandbox so that the engine's commands are checked against
// the real poppler and Tesseract.
type localBox struct{ dir string }

func (l localBox) path(ws, p string) string { return filepath.Join(l.dir, ws, filepath.Clean("/"+p)) }

func (l localBox) Run(ctx context.Context, r sandbox.Request) (*sandbox.Result, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", r.Command)
	cmd.Dir = filepath.Join(l.dir, r.Workspace)
	_ = os.MkdirAll(cmd.Dir, 0o755)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		return nil, err
	}
	return &sandbox.Result{ExitCode: code, Output: string(out)}, nil
}
func (l localBox) Reset(context.Context, string) error { return nil }

func (l localBox) Put(_ context.Context, ws, p string, r io.Reader, max int64) error {
	dst := l.path(ws, p)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
func (l localBox) Get(_ context.Context, ws, p string, max int64) ([]byte, error) {
	b, err := os.ReadFile(l.path(ws, p))
	if errors.Is(err, os.ErrNotExist) {
		return nil, sandbox.ErrNotFound
	}
	return b, err
}
func (l localBox) Remove(_ context.Context, ws, p string) error { return os.Remove(l.path(ws, p)) }

func haveTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s is not installed", n)
		}
	}
}

func engines(t *testing.T, kind string) []PDFEngine {
	switch kind {
	case "native":
		return []PDFEngine{NativeEngine{}}
	case "sandbox":
		haveTools(t, "pdfinfo", "pdftotext", "pdftoppm", "tesseract")
		box := localBox{dir: t.TempDir()}
		return []PDFEngine{&SandboxEngine{Runner: box, Files: box}}
	}
	t.Fatal(kind)
	return nil
}

func newProc(t *testing.T, kind string, cfg Config) *Processor {
	return New(cfg, quiet(), engines(t, kind)...)
}

func process(p *Processor, name string, data []byte) (*Result, error) {
	return p.Process(bg, sandbox.WorkspaceID("test"), Input{Name: name, Data: data})
}

func forEngines(t *testing.T, f func(t *testing.T, kind string)) {
	for _, kind := range []string{"native", "sandbox"} {
		t.Run(kind, func(t *testing.T) { f(t, kind) })
	}
}

func TestTextPDF(t *testing.T) {
	forEngines(t, func(t *testing.T, kind string) {
		p := newProc(t, kind, Config{})
		res, err := process(p, "text-en.pdf", fixture(t, "text-en.pdf"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Kind != KindPDF || res.Pages != 2 || !res.Inline || res.Original == nil {
			t.Errorf("res = %+v", res)
		}
		pages := strings.Split(res.Text, PageSep)
		if len(pages) != 2 || !strings.Contains(pages[0], "marmalade project shipped") || !strings.Contains(pages[1], "zeppelin inspection") {
			t.Errorf("pages = %q", pages)
		}
		if strings.Contains(pages[0], "zeppelin") {
			t.Error("text of page 2 leaked into page 1")
		}
	})
}

func TestThaiTextPDF(t *testing.T) {
	forEngines(t, func(t *testing.T, kind string) {
		p := newProc(t, kind, Config{})
		res, err := process(p, "text-th.pdf", fixture(t, "text-th.pdf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Text, "งบประมาณที่ใช้ 4200 บาท") {
			t.Errorf("text = %q", res.Text)
		}
	})
}

func TestLongTextIsNotInlined(t *testing.T) {
	p := newProc(t, "native", Config{InlineChars: 20})
	res, err := process(p, "text-en.pdf", fixture(t, "text-en.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Inline || res.Chars < 50 {
		t.Errorf("inline = %v, chars = %d", res.Inline, res.Chars)
	}
}

func TestScannedPDFWithOCRAndVision(t *testing.T) {
	haveTools(t, "pdfinfo", "tesseract")
	p := newProc(t, "sandbox", Config{OCRLangs: "eng+tha", Vision: true})
	res, err := process(p, "scanned-en.pdf", fixture(t, "scanned-en.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) == 0 || !strings.Contains(res.Note, "OCR") {
		t.Errorf("images = %d, note = %q", len(res.Images), res.Note)
	}
	if _, err := jpeg.Decode(bytes.NewReader(res.Images[0])); err != nil {
		t.Errorf("the page picture is not a JPEG: %v", err)
	}
	if nonBlank(res.Text) < 10 {
		t.Errorf("OCR found no text: %q", res.Text)
	}
	t.Logf("OCR: %q", res.Text)

	// without a vision model only the OCR text is used
	p = newProc(t, "sandbox", Config{OCRLangs: "eng+tha"})
	res, err = process(p, "scanned-en.pdf", fixture(t, "scanned-en.pdf"))
	if err != nil || len(res.Images) != 0 || nonBlank(res.Text) < 10 {
		t.Errorf("no vision: res = %+v, err = %v", res, err)
	}
}

func TestScannedThaiOCRRepairsSpacing(t *testing.T) {
	haveTools(t, "pdfinfo", "tesseract")
	p := newProc(t, "sandbox", Config{OCRLangs: "eng+tha"})
	res, err := process(p, "scanned-th.pdf", fixture(t, "scanned-th.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("OCR: %q", res.Text)
	if !strings.Contains(res.Text, "ยอดรวม") || !strings.Contains(res.Text, "1250") {
		t.Errorf("text = %q", res.Text)
	}
}

func TestScannedPDFWithoutOCRIsRefusedClearly(t *testing.T) {
	p := newProc(t, "native", Config{Vision: true})
	if _, err := process(p, "scanned-en.pdf", fixture(t, "scanned-en.pdf")); !errors.Is(err, ErrScanned) {
		t.Errorf("err = %v", err)
	}
}

func TestPDFProblems(t *testing.T) {
	forEngines(t, func(t *testing.T, kind string) {
		p := newProc(t, kind, Config{PDFMaxPages: 30})
		if _, err := process(p, "e.pdf", fixture(t, "encrypted.pdf")); !errors.Is(err, ErrEncrypted) {
			t.Errorf("encrypted: %v", err)
		}
		if _, err := process(p, "c.pdf", fixture(t, "corrupt.pdf")); !errors.Is(err, ErrCorrupt) {
			t.Errorf("corrupt: %v", err)
		}
		if _, err := process(p, "m.pdf", fixture(t, "many-pages.pdf")); !errors.Is(err, ErrTooManyPages) {
			t.Errorf("many pages: %v", err)
		}
	})
}

func TestSandboxEngineCleansUpAndFallsBack(t *testing.T) {
	haveTools(t, "pdfinfo", "pdftotext", "pdftoppm", "tesseract")
	box := localBox{dir: t.TempDir()}
	ws := sandbox.WorkspaceID("test")
	p := New(Config{}, quiet(), &SandboxEngine{Runner: box, Files: box}, NativeEngine{})
	if _, err := p.Process(bg, ws, Input{Name: "a.pdf", Data: fixture(t, "text-en.pdf")}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(box.dir, ws, ".attach"))
	if len(entries) != 0 {
		t.Errorf("scratch files left behind: %v", entries)
	}
	// a sandbox that cannot be used hands over to the next engine
	broken := &SandboxEngine{Runner: failingRunner{}, Files: box}
	p = New(Config{}, quiet(), broken, NativeEngine{})
	res, err := p.Process(bg, ws, Input{Name: "a.pdf", Data: fixture(t, "text-en.pdf")})
	if err != nil || !strings.Contains(res.Text, "marmalade") {
		t.Errorf("fallback: %v %+v", err, res)
	}
	// but a verdict of "encrypted" is not retried elsewhere
	p = New(Config{}, quiet(), &SandboxEngine{Runner: box, Files: box}, NativeEngine{})
	if _, err := p.Process(bg, ws, Input{Name: "e.pdf", Data: fixture(t, "encrypted.pdf")}); !errors.Is(err, ErrEncrypted) {
		t.Errorf("encrypted: %v", err)
	}
}

type failingRunner struct{}

func (failingRunner) Run(context.Context, sandbox.Request) (*sandbox.Result, error) {
	return nil, sandbox.ErrBusy
}
func (failingRunner) Reset(context.Context, string) error { return nil }

func TestImageProcessing(t *testing.T) {
	p := New(Config{Vision: true, ImageMaxEdge: 1568}, quiet())
	res, err := process(p, "big.jpg", fixture(t, "big.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(res.Images[0]))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); max(b.Dx(), b.Dy()) != 1568 {
		t.Errorf("size = %v", b)
	}
	if res.Original != nil || res.MIME != "image/jpeg" {
		t.Errorf("a picture keeps only the re-encoded copy: %+v", res)
	}
}

func TestImageFormats(t *testing.T) {
	p := New(Config{Vision: true}, quiet())
	for _, name := range []string{"alpha.png", "anim.gif", "pic.webp", "photo-exif6.jpg"} {
		res, err := process(p, name, fixture(t, name))
		if err != nil || len(res.Images) != 1 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestImageAlphaIsFlattenedOnWhite(t *testing.T) {
	p := New(Config{Vision: true}, quiet())
	res, err := process(p, "alpha.png", fixture(t, "alpha.png"))
	if err != nil {
		t.Fatal(err)
	}
	img, _ := jpeg.Decode(bytes.NewReader(res.Images[0]))
	src, _ := png.Decode(bytes.NewReader(fixture(t, "alpha.png")))
	// find a fully transparent pixel in the source and check it came out white
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := src.At(x, y).RGBA(); a == 0 {
				r, g, bl, _ := img.At(x, y).RGBA()
				if r>>8 < 240 || g>>8 < 240 || bl>>8 < 240 {
					t.Fatalf("transparent pixel (%d,%d) became %v", x, y, color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), 255})
				}
				return
			}
		}
	}
	t.Skip("fixture has no transparent pixel")
}

func TestEXIFOrientationIsApplied(t *testing.T) {
	// photo-exif6.jpg is stored landscape with orientation 6 (rotate 90° CW)
	src, err := jpeg.Decode(bytes.NewReader(fixture(t, "photo-exif6.jpg")))
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Vision: true}, quiet())
	res, err := process(p, "photo-exif6.jpg", fixture(t, "photo-exif6.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := jpeg.Decode(bytes.NewReader(res.Images[0]))
	if out.Bounds().Dx() != src.Bounds().Dy() || out.Bounds().Dy() != src.Bounds().Dx() {
		t.Errorf("stored %v, sent %v: the picture was not rotated", src.Bounds(), out.Bounds())
	}
}

func TestOrientationTransforms(t *testing.T) {
	// a 3x2 picture with a distinct colour in every pixel
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := 0; i < 6; i++ {
		src.Set(i%3, i/3, color.RGBA{uint8(i*40 + 10), uint8(255 - i*40), 7, 255})
	}
	// where the top-left source pixel (0,0) ends up, and the output size
	cases := []struct {
		o, w, h int
		x, y    int
	}{
		{1, 3, 2, 0, 0}, {2, 3, 2, 2, 0}, {3, 3, 2, 2, 1}, {4, 3, 2, 0, 1},
		{5, 2, 3, 0, 0}, {6, 2, 3, 1, 0}, {7, 2, 3, 1, 2}, {8, 2, 3, 0, 2},
	}
	for _, c := range cases {
		out := orient(src, c.o)
		if out.Bounds().Dx() != c.w || out.Bounds().Dy() != c.h {
			t.Errorf("orientation %d: size %v", c.o, out.Bounds())
			continue
		}
		if got, want := out.At(c.x, c.y), src.At(0, 0); got != want {
			t.Errorf("orientation %d: source (0,0) is not at (%d,%d)", c.o, c.x, c.y)
		}
	}
}

func TestImageLimits(t *testing.T) {
	p := New(Config{Vision: true, MaxImagePixels: 1_000_000}, quiet())
	if _, err := process(p, "bomb.png", fixture(t, "bomb.png")); !errors.Is(err, ErrImageTooBig) {
		t.Errorf("bomb: %v", err)
	}
	// a PNG header followed by garbage
	bad := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1, 2, 3}, 100)...)
	if _, err := process(p, "x.png", bad); !errors.Is(err, ErrCorrupt) {
		t.Errorf("garbage png: %v", err)
	}
	truncated := fixture(t, "big.jpg")
	truncated = truncated[:len(truncated)/3]
	if _, err := process(p, "t.jpg", truncated); err == nil {
		t.Log("a truncated JPEG was accepted (the decoder tolerates it)")
	}
}

func TestVisionOff(t *testing.T) {
	p := New(Config{}, quiet())
	if _, err := process(p, "a.png", fixture(t, "alpha.png")); !errors.Is(err, ErrVisionOff) {
		t.Errorf("err = %v", err)
	}
	// documents still work
	if _, err := process(p, "a.txt", []byte("hello there")); err != nil {
		t.Errorf("text: %v", err)
	}
}

func TestSniffingIgnoresNameAndDeclaredType(t *testing.T) {
	p := New(Config{Vision: true}, quiet(), NativeEngine{})
	// a PDF named .txt, an image named .pdf
	res, err := p.Process(bg, "w", Input{Name: "notes.txt", MIME: "text/plain", Data: fixture(t, "text-en.pdf")})
	if err != nil || res.Kind != KindPDF {
		t.Errorf("pdf named txt: %v %+v", err, res)
	}
	res, err = p.Process(bg, "w", Input{Name: "scan.pdf", MIME: "application/pdf", Data: fixture(t, "alpha.png")})
	if err != nil || res.Kind != KindImage {
		t.Errorf("png named pdf: %v %+v", err, res)
	}
	// an executable is refused whatever it is called
	elf := append([]byte("\x7fELF"), make([]byte, 100)...)
	if _, err := p.Process(bg, "w", Input{Name: "readme.txt", MIME: "text/plain", Data: elf}); !errors.Is(err, ErrNotText) {
		t.Errorf("elf: %v", err)
	}
	if _, err := p.Process(bg, "w", Input{Name: "x.zip", Data: []byte("PK\x03\x04....")}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("zip: %v", err)
	}
}

func TestSizeLimitAndEmpty(t *testing.T) {
	p := New(Config{MaxBytes: 100}, quiet())
	if _, err := process(p, "a.txt", bytes.Repeat([]byte("a"), 101)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v", err)
	}
	if _, err := process(p, "a.txt", nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("empty: %v", err)
	}
}

func TestTextFiles(t *testing.T) {
	p := New(Config{PageChars: 100, InlineChars: 500}, quiet())
	res, err := process(p, "a.csv", []byte("\xef\xbb\xbfname,qty\r\nbolt,3\r\n"))
	if err != nil || res.Text != "name,qty\nbolt,3\n" || !res.Inline || res.Pages != 1 {
		t.Errorf("utf-8 bom: %v %+v", err, res)
	}
	res, err = process(p, "thai.csv", fixture(t, "thai-tis620.csv"))
	if err != nil || !strings.Contains(res.Text, "แอปเปิ้ล") {
		t.Errorf("tis-620: %v %q", err, res.Text)
	}
	if !strings.Contains(res.Note, "874") {
		t.Errorf("note = %q", res.Note)
	}
	u16 := []byte{0xff, 0xfe, 'h', 0, 'i', 0, '\n', 0}
	res, err = process(p, "u.txt", u16)
	if err != nil || strings.TrimSpace(res.Text) != "hi" {
		t.Errorf("utf-16: %v %q", err, res.Text)
	}
	// pages
	var long strings.Builder
	for i := 0; i < 50; i++ {
		long.WriteString("line number " + strings.Repeat("x", 8) + "\n")
	}
	res, err = process(p, "long.log", []byte(long.String()))
	if err != nil || res.Pages < 10 || res.Inline {
		t.Errorf("paging: %v pages=%d inline=%v", err, res.Pages, res.Inline)
	}
	for _, pg := range strings.Split(res.Text, PageSep) {
		if len([]rune(pg)) > 100 {
			t.Errorf("page of %d characters", len([]rune(pg)))
		}
	}
	if strings.Replace(res.Text, PageSep, "", -1) != long.String() {
		t.Error("paging lost or changed text")
	}
	// binary rubbish named .txt
	if _, err := process(p, "a.txt", []byte("abc\x00\x01\x02def")); !errors.Is(err, ErrNotText) {
		t.Errorf("binary: %v", err)
	}
	// control characters and the page separator are not passed through
	res, _ = process(p, "a.txt", []byte("a\fb\x07c\u202ed\n"))
	if strings.Contains(res.Text, "\x07") || strings.Contains(res.Text, "\u202e") || strings.Contains(res.Text, PageSep) {
		t.Errorf("text = %q", res.Text)
	}
}

func TestVeryLongLineIsSplit(t *testing.T) {
	p := New(Config{PageChars: 50}, quiet())
	res, err := process(p, "min.json", []byte(`{"a":"`+strings.Repeat("z", 170)+`"}`))
	if err != nil || res.Pages != 4 {
		t.Errorf("%v pages=%d", err, res.Pages)
	}
}

func TestMaxTextChars(t *testing.T) {
	p := New(Config{MaxTextChars: 30, PageChars: 1000}, quiet())
	res, err := process(p, "a.txt", []byte(strings.Repeat("abcdefghij", 10)))
	if err != nil || res.Chars != 30 || !strings.Contains(res.Note, "truncated") {
		t.Errorf("%v %+v", err, res)
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                       "report.pdf",
		"../../etc/passwd":                 "passwd",
		`C:\Users\me\secret.txt`:           "secret.txt",
		"a\nb\x00c.txt":                    "abc.txt",
		"":                                 "file",
		"..":                               "..",
		"[2025-01-01T00:00:00Z] Bob#1234:": "[2025-01-01T00:00:00Z] Bob#1234:",
	}
	for in, want := range cases {
		if got := cleanName(in); got != want && !(in == ".." && got == "file") {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("ก", 200) + ".pdf"
	if got := cleanName(long); len([]rune(got)) != 80 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name: %d runes %q", len([]rune(got)), got)
	}
}

func TestCleanOCRThaiSpacing(t *testing.T) {
	got := cleanOCR("ย อ ด ร ว ม 1250 บ า ท\nHello world and more")
	if got != "ยอดรวม 1250 บาท\nHello world and more" {
		t.Errorf("got %q", got)
	}
	// ordinary Thai with spaces between phrases is left alone
	keep := "สวัสดีครับ ยินดีที่ได้รู้จัก วันนี้อากาศดี"
	if got := cleanOCR(keep); got != keep {
		t.Errorf("changed normal text: %q", got)
	}
}

func TestConcurrencyLimitAndCancel(t *testing.T) {
	p := New(Config{Concurrency: 1}, quiet(), slowEngine{})
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := p.Process(bg, "w", Input{Name: "a.pdf", Data: fixture(t, "text-en.pdf"), MIME: ""})
		done <- err
	}()
	<-started2(p, started)
	ctx, cancel := context.WithTimeout(bg, 100*time.Millisecond)
	defer cancel()
	// the only slot is taken: the second caller waits and gives up with its context
	if _, err := p.Process(ctx, "w", Input{Name: "b.txt", Data: []byte("hi")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	slowRelease <- struct{}{}
	if err := <-done; err != nil {
		t.Errorf("first: %v", err)
	}
}

var slowRelease = make(chan struct{})

// started2 waits until the slow engine holds the only slot.
func started2(p *Processor, _ chan struct{}) <-chan struct{} {
	c := make(chan struct{})
	go func() {
		for len(p.slots) == 0 {
			time.Sleep(time.Millisecond)
		}
		close(c)
	}()
	return c
}

type slowEngine struct{}

func (slowEngine) Name() string { return "slow" }
func (slowEngine) Open(ctx context.Context, _ string, data []byte) (PDFDoc, error) {
	<-slowRelease
	return NativeEngine{}.Open(ctx, "", data)
}

var _ = draw.Src

func TestInboxPath(t *testing.T) {
	cases := []struct {
		id         int64
		name, kind string
		want       string
	}{
		{7, "report.pdf", KindPDF, "inbox/7-report.pdf"},
		{8, "My Report (final).PDF", KindPDF, "inbox/8-My_Report_final.pdf"},
		{9, "รายงาน.pdf", KindPDF, "inbox/9-file.pdf"},
		{10, "photo.png", KindImage, "inbox/10-photo.jpg"},
		{11, "../../etc/passwd", KindText, "inbox/11-passwd"},
		{12, "a;rm -rf $HOME.txt", KindText, "inbox/12-a_rm_-rf_HOME.txt"},
		{13, "noext", KindText, "inbox/13-noext"},
	}
	for _, c := range cases {
		got := InboxPath(c.id, cleanName(c.name), c.kind)
		if got != c.want {
			t.Errorf("InboxPath(%q) = %q, want %q", c.name, got, c.want)
		}
		if strings.ContainsAny(got[len("inbox/"):], "/\\ ;$'\"`&|<>*?") {
			t.Errorf("%q needs quoting", got)
		}
	}
}

// A file longer than the sample looked at is cut wherever the sample ends, often
// in the middle of a Thai character (three bytes). That must not make valid text
// look like binary data.
func TestLongUTF8TextIsAcceptedWhereverTheSampleEnds(t *testing.T) {
	p := New(Config{}, quiet())
	line := "การตั้งค่าระบบปฏิบัติการและเครื่องมือสำหรับนักพัฒนา 🚀 ├── src\n"
	for pad := 0; pad < 8; pad++ {
		body := strings.Repeat("a", pad) + strings.Repeat(line, 300)
		if len(body) < 3*sampleSize {
			t.Fatal("the test file is too short to be cut")
		}
		res, err := process(p, "OS1.md", []byte(body))
		if err != nil || !strings.Contains(res.Text, "การตั้งค่าระบบ") {
			t.Errorf("pad %d: %v", pad, err)
		}
		if res != nil && strings.ContainsRune(res.Text, '\uFFFD') {
			t.Errorf("pad %d: valid text came out damaged", pad)
		}
	}
}

func TestTextWithAFewDamagedBytesIsStillText(t *testing.T) {
	p := New(Config{}, quiet())
	body := strings.Repeat("สวัสดีครับ ยินดีต้อนรับ — ├── tree\n", 200)
	data := []byte(body[:500] + "\x96" + body[500:]) // one stray byte, as pasted text often has
	res, err := process(p, "notes.md", data)
	if err != nil || !strings.Contains(res.Text, "สวัสดีครับ") || !strings.Contains(res.Note, "damaged") {
		t.Fatalf("%v %+v", err, res)
	}
	// the Thai text next to the stray byte is still decoded as UTF-8, not as Windows-874
	if strings.Contains(res.Text, "à¸") || strings.Contains(res.Text, "เธ") {
		t.Errorf("UTF-8 was decoded as another encoding: %q", res.Text[:80])
	}
}

func TestTextEncodings(t *testing.T) {
	long := strings.Repeat("x", 9000)
	for name, c := range map[string]struct {
		data []byte
		want string
	}{
		"empty":              {nil, "utf-8"},
		"ascii":              {[]byte("hello"), "utf-8"},
		"bom":                {append([]byte{0xef, 0xbb, 0xbf}, "x"...), "utf-8"},
		"utf-16":             {[]byte{0xff, 0xfe, 'h', 0}, "utf-16"},
		"nul":                {[]byte("abc\x00def"), ""},
		"nul far away":       {[]byte(long + "\x00"), "utf-8"}, // beyond the sample: not looked at
		"tis-620":            {[]byte("\xa1\xd2\xc3\xb7\xb4\xcb\xc5\xd2\xc2 \xa1\xd2\xc3"), "windows-874"},
		"binary":             {[]byte("\x89\x01\x02\xfe\xfd\xfc\xfb\xfa\xf9"), ""},
		"cut after one byte": {[]byte(strings.Repeat("a", sampleSize-1) + "ก"), "utf-8"},
		"cut after two":      {[]byte(strings.Repeat("a", sampleSize-2) + "ก"), "utf-8"},
	} {
		if got := textEncoding(c.data); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
