package knowledge

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/authapon/jannyq/internal/attach"
	"github.com/authapon/jannyq/internal/metrics"
)

// bagEmbedder embeds text as a hashed bag of words, so that texts sharing words
// are similar. It counts calls and can be made to fail.
type bagEmbedder struct {
	mu     sync.Mutex
	calls  int
	texts  int
	models []string
	fail   bool
}

func (b *bagEmbedder) Embed(_ context.Context, model string, in []string) ([][]float32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.texts += len(in)
	b.models = append(b.models, model)
	if b.fail {
		return nil, errors.New("embedding service down")
	}
	out := make([][]float32, len(in))
	for i, s := range in {
		v := make([]float32, 4096)
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			h := fnv.New32a()
			h.Write([]byte(w))
			v[h.Sum32()%4096]++
		}
		out[i] = v
	}
	return out, nil
}

func (b *bagEmbedder) count() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.texts
}

type rig struct {
	t   *testing.T
	dir string
	ix  *Indexer
	kb  *KB
	emb *bagEmbedder
	now time.Time
}

func newRig(t *testing.T, withEmbedder bool) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir(), now: time.Now().Add(time.Hour)}
	r.kb = &KB{Store: openStore(t), MinCosine: 0.2}
	if withEmbedder {
		r.emb = &bagEmbedder{}
		r.kb.Embedder, r.kb.Model = r.emb, "m1"
	}
	proc := attach.New(attach.Config{PageChars: 1 << 30, MaxTextChars: 1 << 26, PDFMaxPages: 100}, slog.New(slog.NewTextHandler(io.Discard, nil)), attach.NativeEngine{})
	ix, err := NewIndexer(IndexerConfig{Dir: r.dir, Processor: proc, Workspace: "kb", ChunkChars: 300, Overlap: 30,
		Settle: time.Second, MaxFileBytes: 1 << 20, EmbedRetry: time.Minute, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, r.kb)
	if err != nil {
		t.Fatal(err)
	}
	ix.now = func() time.Time { return r.now }
	r.ix = ix
	return r
}

func (r *rig) write(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	// well in the past: settled
	old := r.now.Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
}

func (r *rig) scan() ScanResult {
	r.t.Helper()
	res, err := r.ix.Scan(bg)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *rig) find(q string) []Hit {
	r.t.Helper()
	res, err := r.kb.Search(bg, q, 10)
	if err != nil {
		r.t.Fatal(err)
	}
	return res.Hits
}

func fixturePDF(t *testing.T, name string) string {
	b, err := os.ReadFile(filepath.Join("..", "attach", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInitialScan(t *testing.T) {
	r := newRig(t, true)
	r.write("notes.txt", "The marmalade recipe needs oranges, sugar and patience.")
	r.write("handbook/policy.md", "# Policy\n\nEmployees must lock their laptops when away from the desk.")
	r.write("handbook/ข้อมูล.txt", "นโยบายการลาพักร้อน พนักงานมีวันลาพักร้อนสิบวันต่อปี")
	r.write("report.pdf", fixturePDF(t, "text-en.pdf"))
	r.write("picture.png", "not indexed")
	r.write("archive.zip", "not indexed")
	r.write(".hidden/secret.txt", "hidden words")
	r.write(".dotfile.txt", "dot words")
	res := r.scan()
	if res.Added != 4 || res.Files != 4 || res.Failed != 0 {
		t.Fatalf("%+v", res)
	}
	for q, want := range map[string]string{
		"marmalade oranges": "notes.txt", "lock laptops": "handbook/policy.md", "วันลาพักร้อน": "handbook/ข้อมูล.txt",
		"zeppelin": "report.pdf", "hidden": "", "dot words": "", "not indexed": "",
	} {
		hits := r.find(q)
		if want == "" {
			for _, h := range hits {
				if h.ByWords {
					t.Errorf("%q matched a file that must not be indexed: %+v", q, h)
				}
			}
			continue
		}
		if len(hits) == 0 || hits[0].Path != want {
			t.Errorf("%q: %v, want %s first", q, paths(hits), want)
		}
	}
	if hits := r.find("zeppelin inspection"); len(hits) == 0 || hits[0].Page != 2 || hits[0].Kind != "pdf" {
		t.Errorf("a PDF passage knows its page: %+v", hits)
	}
	if hits := r.find("marmalade"); hits[0].Page != 0 {
		t.Errorf("a text passage has no page: %+v", hits[0])
	}
	// the second scan changes nothing and reads nothing
	calls, _ := r.emb.count()
	if res = r.scan(); res.Changed() {
		t.Errorf("second scan: %+v", res)
	}
	if c2, _ := r.emb.count(); c2 != calls {
		t.Error("an unchanged folder was embedded again")
	}
}

func TestEditDeleteRename(t *testing.T) {
	r := newRig(t, true)
	r.write("a.txt", "Original content about aardvarks and anteaters.")
	r.write("b.txt", "Different content about badgers and bison, quite unrelated.")
	r.scan()

	// edit
	r.write("a.txt", "Rewritten content about armadillos only.")
	res := r.scan()
	if res.Edited != 1 || res.Added != 0 {
		t.Errorf("%+v", res)
	}
	if len(r.find("aardvarks")) != 0 {
		t.Error("the old text is still found")
	}
	if h := r.find("armadillos"); len(h) != 1 || h[0].Path != "a.txt" {
		t.Errorf("%v", h)
	}

	// touching without changing the content costs no embedding
	_, texts := r.emb.count()
	touched := r.now.Add(-30 * time.Minute)
	_ = os.Chtimes(filepath.Join(r.dir, "b.txt"), touched, touched)
	res = r.scan()
	_, t2 := r.emb.count()
	if res.Edited != 0 || res.Added != 0 {
		t.Errorf("a touch counted as an edit: %+v", res)
	}
	if t2 != texts {
		t.Error("a touched file was embedded again")
	}
	files, _ := r.kb.Store.Files(bg)
	for _, f := range files {
		if f.Path == "b.txt" && f.MtimeNS != touched.UnixNano() {
			t.Error("the new time was not recorded, so the file would be hashed on every scan")
		}
	}

	// rename keeps the passages and does not embed
	_, texts = r.emb.count()
	if err := os.MkdirAll(filepath.Join(r.dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(r.dir, "b.txt"), filepath.Join(r.dir, "sub", "moved.txt")); err != nil {
		t.Fatal(err)
	}
	res = r.scan()
	_, t2 = r.emb.count()
	if res.Renamed != 1 || res.Removed != 0 || res.Added != 0 {
		t.Errorf("%+v", res)
	}
	if h := r.find("badgers"); len(h) != 1 || h[0].Path != "sub/moved.txt" {
		t.Errorf("%v", h)
	}
	if t2 != texts {
		t.Error("a renamed file was embedded again")
	}

	// delete
	_ = os.Remove(filepath.Join(r.dir, "sub", "moved.txt"))
	if res = r.scan(); res.Removed != 1 {
		t.Errorf("%+v", res)
	}
	if len(r.find("badgers")) != 0 {
		t.Error("a deleted file is still found")
	}
	// delete the whole folder content
	_ = os.Remove(filepath.Join(r.dir, "a.txt"))
	r.scan()
	if st, _ := r.kb.Store.Stats(bg); st.Files != 0 || st.Chunks != 0 || st.Embedded != 0 {
		t.Errorf("leftovers: %+v", st)
	}
}

func TestAFileStillBeingWrittenWaits(t *testing.T) {
	r := newRig(t, false)
	r.write("fresh.txt", "Content that was just written to disk now.")
	_ = os.Chtimes(filepath.Join(r.dir, "fresh.txt"), r.now, r.now) // modified "just now"
	res := r.scan()
	if res.Added != 0 || res.Unsettled != 1 {
		t.Errorf("%+v", res)
	}
	r.now = r.now.Add(5 * time.Second)
	if res = r.scan(); res.Added != 1 {
		t.Errorf("%+v", res)
	}
}

func TestUnreadableFilesAreRecordedOnceAndRetriedWhenChanged(t *testing.T) {
	r := newRig(t, false)
	r.write("broken.pdf", fixturePDF(t, "corrupt.pdf"))
	r.write("locked.pdf", fixturePDF(t, "encrypted.pdf"))
	r.write("scan.pdf", fixturePDF(t, "scanned-en.pdf"))
	r.write("huge.txt", strings.Repeat("x", 2<<20))
	r.write("empty.txt", "   \n  ")
	r.write("good.txt", "A perfectly fine file with words in it.")
	res := r.scan()
	if res.Added != 1 || res.Failed != 5 {
		t.Fatalf("%+v", res)
	}
	files, _ := r.kb.Store.Files(bg)
	reasons := map[string]string{}
	for _, f := range files {
		if f.Status == "error" {
			reasons[f.Path] = f.Error
		}
	}
	for p, want := range map[string]string{"broken.pdf": "damaged", "locked.pdf": "password", "scan.pdf": "scanned", "huge.txt": "too large", "empty.txt": "damaged or empty"} {
		if !strings.Contains(reasons[p], want) {
			t.Errorf("%s: reason %q lacks %q", p, reasons[p], want)
		}
	}
	if st, _ := r.kb.Store.Stats(bg); st.Files != 1 || st.Failed != 5 {
		t.Errorf("%+v", st)
	}
	// not retried while unchanged
	if res = r.scan(); res.Failed != 0 || res.Changed() {
		t.Errorf("%+v", res)
	}
	// fixed: indexed
	r.write("broken.pdf", fixturePDF(t, "text-en.pdf"))
	if res = r.scan(); res.Edited != 1 {
		t.Errorf("%+v", res)
	}
	if h := r.find("marmalade"); len(h) == 0 || h[0].Path != "broken.pdf" {
		t.Errorf("%v", h)
	}
	if files, _ = r.kb.Store.Files(bg); func() bool {
		for _, f := range files {
			if f.Path == "broken.pdf" && f.Status != "ok" {
				return true
			}
		}
		return false
	}() {
		t.Error("the error status was not cleared")
	}
}

func TestSymlinksAndEscapesAreNotFollowed(t *testing.T) {
	r := newRig(t, false)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret outside words"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.write("inside.txt", "ordinary inside words here")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(r.dir, "link.txt")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink(outside, filepath.Join(r.dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../etc/passwd", filepath.Join(r.dir, "passwd.txt")); err != nil {
		t.Fatal(err)
	}
	res := r.scan()
	if res.Added != 1 || res.Failed != 0 {
		t.Errorf("%+v", res)
	}
	if len(r.find("secret")) != 0 || len(r.find("root:")) != 0 {
		t.Error("a file outside the folder was indexed through a link")
	}
}

func TestEmbeddingFailureDegradesToWordsAndRecovers(t *testing.T) {
	r := newRig(t, true)
	r.emb.fail = true
	r.write("a.txt", "Aardvark facts: they eat ants at night.")
	r.write("b.txt", "Badger facts: they dig large burrows.")
	r.write("c.txt", "Capybara facts: they love water.")
	res := r.scan()
	if res.Added != 3 {
		t.Fatalf("%+v", res)
	}
	// only the first failure is tried: the service is not hammered once per file
	if calls, _ := r.emb.count(); calls != 1 {
		t.Errorf("%d embedding calls while the service is down", calls)
	}
	if h := r.find("burrows"); len(h) != 1 || h[0].Path != "b.txt" {
		t.Errorf("words must still work: %v", h)
	}
	if st, _ := r.kb.Store.Stats(bg); st.Embedded != 0 {
		t.Errorf("%+v", st)
	}
	// before the retry time nothing is attempted
	r.emb.fail = false
	if res = r.scan(); res.Embedded != 0 {
		t.Errorf("%+v", res)
	}
	// after it, passages are embedded from what is stored, without re-reading the files
	r.now = r.now.Add(2 * time.Minute)
	if res = r.scan(); res.Embedded != 3 || res.Added != 0 || res.Edited != 0 {
		t.Errorf("%+v", res)
	}
	if st, _ := r.kb.Store.Stats(bg); st.Embedded != st.Chunks || st.Chunks != 3 {
		t.Errorf("%+v", st)
	}
	// and semantic search works: a question sharing words with the passage but not matching exactly
	res2, err := r.kb.Search(bg, "what do capybaras love", 3)
	if err != nil || !res2.Semantic || len(res2.Hits) == 0 || res2.Hits[0].Path != "c.txt" {
		t.Errorf("%+v %v", res2, err)
	}
}

func TestChangingTheEmbeddingModelEmbedsAgain(t *testing.T) {
	r := newRig(t, true)
	r.write("a.txt", "Some knowledge about turbines and generators.")
	r.scan()
	r.kb.Model = "m2"
	res := r.scan()
	if res.Embedded != 1 || res.Edited != 0 {
		t.Errorf("%+v", res)
	}
	r.emb.mu.Lock()
	last := r.emb.models[len(r.emb.models)-1]
	r.emb.mu.Unlock()
	if last != "m2" {
		t.Errorf("last model = %q", last)
	}
	files, _ := r.kb.Store.Files(bg)
	if files[0].EmbedModel != "m2" {
		t.Errorf("model = %q", files[0].EmbedModel)
	}
	// embeddings switched off: vectors are dropped, words keep working
	r.kb.Embedder, r.kb.Model = nil, ""
	if res = r.scan(); res.Embedded != 1 {
		t.Errorf("%+v", res)
	}
	if st, _ := r.kb.Store.Stats(bg); st.Embedded != 0 || st.Chunks == 0 {
		t.Errorf("%+v", st)
	}
	if h := r.find("turbines"); len(h) != 1 {
		t.Errorf("%v", h)
	}
	if res = r.scan(); res.Changed() {
		t.Errorf("a settled index keeps changing: %+v", res)
	}
}

func TestSearchFallsBackToWordsWhenTheQuestionCannotBeEmbedded(t *testing.T) {
	r := newRig(t, true)
	r.write("a.txt", "Seagulls nest on cliffs near the harbour.")
	r.scan()
	r.emb.fail = true
	res, err := r.kb.Search(bg, "seagulls", 3)
	if err != nil || res.Semantic || res.Note == "" || len(res.Hits) != 1 {
		t.Errorf("%+v %v", res, err)
	}
}

func TestRunWatchesTheFolder(t *testing.T) {
	r := newRig(t, false)
	r.ix.cfg.Interval = 50 * time.Millisecond
	r.ix.now = time.Now
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { r.ix.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	write := func(name, text string) {
		p := filepath.Join(r.dir, name)
		_ = os.WriteFile(p, []byte(text), 0o644)
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(p, old, old)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if cond() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	write("one.txt", "Flamingos stand on one leg.")
	waitFor("the new file", func() bool { return len(r.find("flamingos")) == 1 })
	write("one.txt", "Pelicans have large beaks.")
	waitFor("the edit", func() bool { return len(r.find("pelicans")) == 1 && len(r.find("flamingos")) == 0 })
	_ = os.Remove(filepath.Join(r.dir, "one.txt"))
	waitFor("the deletion", func() bool { return len(r.find("pelicans")) == 0 })
	if p := r.ix.Progress(); p.LastScan.IsZero() {
		t.Error("no scan recorded")
	}
}

func TestScanNowDoesNotWaitForTheInterval(t *testing.T) {
	r := newRig(t, false)
	r.ix.cfg.Interval = time.Hour
	r.ix.now = time.Now
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { r.ix.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(150 * time.Millisecond) // the first scan finds an empty folder
	p := filepath.Join(r.dir, "two.txt")
	_ = os.WriteFile(p, []byte("Toucans have colourful bills."), 0o644)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	r.ix.ScanNow()
	for i := 0; i < 200; i++ {
		if len(r.find("toucans")) == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("ScanNow did not trigger a scan")
}

func TestNewIndexerRequiresItsParts(t *testing.T) {
	if _, err := NewIndexer(IndexerConfig{}, &KB{}); err == nil {
		t.Error("an empty configuration was accepted")
	}
}

func TestMissingFolderIsAnError(t *testing.T) {
	r := newRig(t, false)
	r.ix.cfg.Dir = filepath.Join(r.dir, "nope")
	if _, err := r.ix.Scan(bg); err == nil {
		t.Error("a missing folder must not look like an empty one (that would forget everything)")
	}
	r.write("keep.txt", "Remember this content please.")
	r.ix.cfg.Dir = r.dir
	r.scan()
	r.ix.cfg.Dir = filepath.Join(r.dir, "nope")
	_, _ = r.ix.Scan(bg)
	if st, _ := r.kb.Store.Stats(bg); st.Files != 1 {
		t.Errorf("an unavailable folder wiped the index: %+v", st)
	}
}

func TestIndexerMetrics(t *testing.T) {
	r := newRig(t, false)
	reg := metrics.New()
	r.ix.cfg.Metrics = metrics.NewInstruments(reg)
	r.ix.cfg.Interval = 20 * time.Millisecond
	r.ix.now = time.Now
	r.write("a.txt", "Some words about kestrels and falcons.")
	r.write("broken.pdf", fixturePDF(t, "corrupt.pdf"))
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { r.ix.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < 200; i++ {
		out := reg.Render()
		if strings.Contains(out, `jannyq_knowledge_changes_total{change="added"} 1`) && strings.Contains(out, `jannyq_knowledge_scans_total{result="ok"}`) {
			if !strings.Contains(out, `change="failed"} 1`) {
				t.Errorf("a failed file is not counted:\n%s", out)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the scan was not counted:\n%s", reg.Render())
}

// Course notes in Thai are long files whose first 8 KB may end inside a character.
func TestLongThaiMarkdownIsIndexedAndBinaryDataGetsAClearReason(t *testing.T) {
	r := newRig(t, false)
	line := "การตั้งค่าระบบปฏิบัติการและเครื่องมือสำหรับนักพัฒนา\n"
	for i := 0; i < 8; i++ {
		r.write(fmt.Sprintf("course/OS%d.md", i), strings.Repeat("a", i)+"# บทที่\n"+strings.Repeat(line, 300))
	}
	r.write("course/broken.md", "# title\n\x00\x01\x02 binary \xff\xfe data")
	res := r.scan()
	if res.Added != 8 || res.Failed != 1 {
		t.Fatalf("%+v", res)
	}
	files, _ := r.kb.Store.Files(bg)
	for _, f := range files {
		if f.Path == "course/broken.md" && !strings.Contains(f.Error, "not readable text") {
			t.Errorf("reason = %q", f.Error)
		}
		if strings.HasPrefix(f.Path, "course/OS") && f.Status != "ok" {
			t.Errorf("%s: %s", f.Path, f.Error)
		}
	}
}
