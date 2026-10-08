package knowledge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/authapon/jannyq/internal/metrics"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/authapon/jannyq/internal/attach"
)

// Extensions of the files that are indexed. Everything else in the folder is
// ignored (listed in the log once per scan at debug level).
var extensions = map[string]bool{
	".pdf": true,
	".txt": true, ".md": true, ".markdown": true, ".rst": true, ".csv": true, ".tsv": true, ".json": true,
	".jsonl": true, ".xml": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".log": true,
	".srt": true, ".vtt": true, ".tex": true,
}

// Supported reports whether a file name is of a type that is indexed.
func Supported(name string) bool { return extensions[strings.ToLower(filepath.Ext(name))] }

// IndexerConfig configures the folder watcher.
type IndexerConfig struct {
	// Dir is the folder to index.
	Dir string
	// Processor reads files (PDF text, OCR, text decoding). Its limits decide
	// what is too large or too long.
	Processor *attach.Processor
	// Workspace is the sandbox workspace used for scratch files when PDFs are read there.
	Workspace string
	// ChunkChars and Overlap shape the passages (defaults 1200 and 150).
	ChunkChars, Overlap int
	// Interval is the time between scans (default 30 s).
	Interval time.Duration
	// Settle is how long a file must be unchanged before it is read, so that a
	// file still being copied is not indexed half-way (default 3 s).
	Settle time.Duration
	// MaxFileBytes skips larger files (default 50 MiB).
	MaxFileBytes int64
	// EmbedBatch is the number of passages embedded per request (default 16).
	EmbedBatch int
	// EmbedRetry is how long to wait before trying again to embed a file after a
	// failure (default 5 minutes).
	EmbedRetry time.Duration
	Log        *slog.Logger
	// Metrics counts scans and changes.
	Metrics metrics.Instruments
}

// Indexer keeps the knowledge base in step with a folder.
type Indexer struct {
	cfg IndexerConfig
	kb  *KB
	log *slog.Logger
	now func() time.Time

	scanMu  sync.Mutex // one scan at a time
	trigger chan struct{}

	embedRetryAt map[string]time.Time // path -> earliest next attempt (scan goroutine only)

	scanning atomic.Bool
	total    atomic.Int64
	done     atomic.Int64
	lastScan atomic.Int64 // unix nanoseconds of the end of the last complete scan
}

// NewIndexer creates the watcher; call Run to start it.
func NewIndexer(cfg IndexerConfig, kb *KB) (*Indexer, error) {
	if cfg.Dir == "" || cfg.Processor == nil || kb == nil {
		return nil, errors.New("knowledge: folder, processor and knowledge base are required")
	}
	if cfg.ChunkChars <= 0 {
		cfg.ChunkChars = 1200
	}
	if cfg.Overlap <= 0 {
		cfg.Overlap = 150
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 3 * time.Second
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 50 << 20
	}
	if cfg.EmbedBatch <= 0 {
		cfg.EmbedBatch = 16
	}
	if cfg.EmbedRetry <= 0 {
		cfg.EmbedRetry = 5 * time.Minute
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Indexer{cfg: cfg, kb: kb, log: log.With("component", "knowledge"), now: time.Now,
		trigger: make(chan struct{}, 1), embedRetryAt: map[string]time.Time{}}, nil
}

// Progress describes what the indexer is doing.
type Progress struct {
	Scanning    bool
	Done, Total int // files handled and files to handle in the current scan
	LastScan    time.Time
}

// Progress reports the state of the current scan.
func (ix *Indexer) Progress() Progress {
	p := Progress{Scanning: ix.scanning.Load(), Done: int(ix.done.Load()), Total: int(ix.total.Load())}
	if n := ix.lastScan.Load(); n > 0 {
		p.LastScan = time.Unix(0, n)
	}
	return p
}

// ScanNow asks the running indexer to scan right away.
func (ix *Indexer) ScanNow() {
	select {
	case ix.trigger <- struct{}{}:
	default:
	}
}

// Run scans the folder now and then every Interval until ctx ends.
func (ix *Indexer) Run(ctx context.Context) {
	for {
		res, err := ix.Scan(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			ix.cfg.Metrics.KnowledgeScans.Inc("error")
			ix.log.Error("scan failed", "dir", ix.cfg.Dir, "err", err)
		case res.Changed():
			for change, n := range map[string]int{"added": res.Added, "edited": res.Edited, "removed": res.Removed,
				"renamed": res.Renamed, "failed": res.Failed, "embedded": res.Embedded} {
				ix.cfg.Metrics.KnowledgeChanges.Add(float64(n), change)
			}
			ix.log.Info("knowledge base updated", "added", res.Added, "changed", res.Edited, "removed", res.Removed,
				"renamed", res.Renamed, "failed", res.Failed, "files", res.Files)
		}
		if err == nil {
			ix.cfg.Metrics.KnowledgeScans.Inc("ok")
		}
		wait := ix.cfg.Interval
		if res.Unsettled > 0 && ix.cfg.Settle+time.Second < wait {
			wait = ix.cfg.Settle + time.Second // a file is still being written: look again soon
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-ix.trigger:
			t.Stop()
		case <-t.C:
		}
	}
}

// ScanResult counts what a scan did.
type ScanResult struct {
	Files     int // files in the folder that are indexed
	Added     int
	Edited    int // edited files re-read
	Removed   int
	Renamed   int
	Failed    int // files that could not be read (with the reason stored)
	Unsettled int // files skipped because they changed moments ago
	Embedded  int // files whose passages were (re-)embedded without being re-read
}

// Changed reports whether the index changed.
func (r ScanResult) Changed() bool {
	return r.Added+r.Edited+r.Removed+r.Renamed+r.Failed+r.Embedded > 0
}

type sourceFile struct {
	rel     string
	size    int64
	mtimeNS int64
}

// walk lists the indexable files of the folder. Symbolic links are not
// followed, and the folder is opened as a root so that nothing outside it can
// be reached, whatever is placed inside it while the scan runs.
func (ix *Indexer) walk(root *os.Root) ([]sourceFile, error) {
	var out []sourceFile
	err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			ix.log.Warn("cannot read", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if p != "." && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() || !Supported(name) {
			return nil // links, devices, and file types that are not indexed
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, sourceFile{rel: p, size: info.Size(), mtimeNS: info.ModTime().UnixNano()})
		return nil
	})
	return out, err
}

// Scan brings the index in line with the folder: new files are read, edited
// files are read again, renamed files keep their passages, files that
// disappeared are forgotten, and passages are (re-)embedded when the embedding
// model changed or an earlier attempt failed.
func (ix *Indexer) Scan(ctx context.Context) (ScanResult, error) {
	ix.scanMu.Lock()
	defer ix.scanMu.Unlock()
	ix.scanning.Store(true)
	defer ix.scanning.Store(false)

	var res ScanResult
	root, err := os.OpenRoot(ix.cfg.Dir)
	if err != nil {
		return res, fmt.Errorf("open the knowledge folder: %w", err)
	}
	defer root.Close()
	files, err := ix.walk(root)
	if err != nil {
		return res, err
	}
	known, err := ix.kb.Store.Files(ctx)
	if err != nil {
		return res, err
	}
	byPath := make(map[string]File, len(known))
	for _, f := range known {
		byPath[f.Path] = f
	}
	present := make(map[string]bool, len(files))
	for _, f := range files {
		present[f.rel] = true
	}
	// files that vanished; a new file with the same content is a rename
	missing := map[string]File{}
	for _, f := range known {
		if !present[f.Path] {
			missing[f.Path] = f
		}
	}

	var todo []sourceFile
	for _, f := range files {
		row, ok := byPath[f.rel]
		if ok && row.Size == f.size && row.MtimeNS == f.mtimeNS && !ix.needsEmbedding(row) {
			continue
		}
		todo = append(todo, f)
	}
	ix.total.Store(int64(len(todo) + len(missing)))
	ix.done.Store(0)
	defer func() { ix.lastScan.Store(ix.now().UnixNano()) }()

	embedDown := false
	for _, f := range todo {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		row, ok := byPath[f.rel]
		unchanged := ok && row.Size == f.size && row.MtimeNS == f.mtimeNS
		if unchanged { // only the embeddings are out of date
			if ix.reembed(ctx, row, &embedDown) {
				res.Embedded++
			}
			ix.done.Add(1)
			continue
		}
		if ix.now().Sub(time.Unix(0, f.mtimeNS)) < ix.cfg.Settle {
			res.Unsettled++
			ix.done.Add(1)
			continue
		}
		outcome := ix.indexFile(ctx, root, f, row, ok, missing, &embedDown)
		switch outcome {
		case outAdded:
			res.Added++
		case outChanged:
			res.Edited++
		case outRenamed:
			res.Renamed++
		case outFailed:
			res.Failed++
		}
		ix.done.Add(1)
	}
	for p := range missing {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if err := ix.kb.Store.Remove(ctx, p); err != nil {
			ix.log.Error("could not forget a deleted file", "path", p, "err", err)
			continue
		}
		delete(ix.embedRetryAt, p)
		res.Removed++
		ix.log.Info("file removed from the knowledge base", "path", p)
		ix.done.Add(1)
	}
	st, _ := ix.kb.Store.Stats(ctx)
	res.Files = st.Files
	return res, nil
}

func (ix *Indexer) needsEmbedding(row File) bool {
	if row.Status != "ok" || row.EmbedModel == ix.kb.Model {
		return false
	}
	if ix.kb.Model != "" && ix.now().Before(ix.embedRetryAt[row.Path]) {
		return false
	}
	return true
}

type outcome int

const (
	outSkipped outcome = iota
	outAdded
	outChanged
	outRenamed
	outFailed
)

// reason is the explanation stored for a file that could not be read.
func reason(err error) string {
	switch {
	case errors.Is(err, attach.ErrCorrupt):
		return "the file is damaged or empty"
	case errors.Is(err, attach.ErrTooLarge):
		return "the file is too large"
	case errors.Is(err, attach.ErrEncrypted):
		return "the PDF is password protected"
	case errors.Is(err, attach.ErrTooManyPages):
		return "the document has too many pages"
	case errors.Is(err, attach.ErrScanned):
		return "a scanned PDF without text, and text recognition is not available"
	case errors.Is(err, attach.ErrNotText):
		return "the file is not readable text: binary data, or a character encoding that is not UTF-8"
	case errors.Is(err, attach.ErrUnsupported):
		return "unsupported type of file"
	}
	return "reading the file failed: " + err.Error()
}

func (ix *Indexer) indexFile(ctx context.Context, root *os.Root, f sourceFile, old File, hadOld bool,
	missing map[string]File, embedDown *bool) outcome {
	rec := File{Path: f.rel, Size: f.size, MtimeNS: f.mtimeNS}
	fail := func(err error) outcome {
		rec.Status, rec.Error = "error", reason(err)
		if ctx.Err() != nil {
			return outSkipped
		}
		if serr := ix.kb.Store.Replace(ctx, rec, nil); serr != nil {
			ix.log.Error("could not record a failed file", "path", f.rel, "err", serr)
		}
		ix.log.Warn("file not indexed", "path", f.rel, "reason", rec.Error)
		return outFailed
	}
	if f.size > ix.cfg.MaxFileBytes {
		return fail(attach.ErrTooLarge)
	}
	data, err := readFile(root, f.rel, ix.cfg.MaxFileBytes)
	if err != nil {
		if errors.Is(err, attach.ErrTooLarge) {
			return fail(err)
		}
		ix.log.Warn("cannot read a file", "path", f.rel, "err", err)
		return outSkipped // vanished or unreadable: try again next time
	}
	sum := sha256.Sum256(data)
	rec.Hash = hex.EncodeToString(sum[:])

	if hadOld && old.Hash == rec.Hash && old.Status == "ok" {
		// touched, not edited
		if err := ix.kb.Store.Touch(ctx, f.rel, f.size, f.mtimeNS); err != nil {
			ix.log.Error("could not update a file", "path", f.rel, "err", err)
		}
		return outSkipped
	}
	if !hadOld {
		for p, m := range missing {
			if m.Hash == rec.Hash && m.Status == "ok" {
				if err := ix.kb.Store.Rename(ctx, p, f.rel, f.size, f.mtimeNS); err != nil {
					ix.log.Error("could not rename a file", "from", p, "to", f.rel, "err", err)
					break
				}
				delete(missing, p)
				ix.log.Info("file renamed in the knowledge base", "from", p, "to", f.rel)
				return outRenamed
			}
		}
	}

	res, err := ix.cfg.Processor.Process(ctx, ix.cfg.Workspace, attach.Input{Name: path.Base(f.rel), Data: data})
	if err != nil {
		return fail(err)
	}
	pages := strings.Split(res.Text, attach.PageSep)
	ext := strings.ToLower(filepath.Ext(f.rel))
	chunks := chunkPages(pages, ix.cfg.ChunkChars, ix.cfg.Overlap, ext == ".md" || ext == ".markdown", res.Kind == attach.KindPDF)
	if len(chunks) == 0 {
		return fail(attach.ErrCorrupt)
	}
	rec.Kind, rec.Pages = res.Kind, len(pages)
	if res.Kind != attach.KindPDF {
		rec.Pages = 0
	}
	if res.Note != "" {
		ix.log.Info("file read with a note", "path", f.rel, "note", res.Note)
	}

	ec := make([]EmbeddedChunk, len(chunks))
	for i, c := range chunks {
		ec[i].Chunk = c
	}
	if ix.kb.Embedder != nil && ix.kb.Model != "" && !*embedDown {
		if vecs, err := ix.embed(ctx, chunks); err != nil {
			if ctx.Err() != nil {
				return outSkipped
			}
			*embedDown = true
			ix.cfg.Metrics.EmbedFailures.Inc()
			ix.embedRetryAt[f.rel] = ix.now().Add(ix.cfg.EmbedRetry)
			ix.log.Warn("could not embed a file; it is searchable by words and will be embedded later", "path", f.rel, "err", err)
		} else {
			for i := range ec {
				ec[i].Vec = vecs[i]
			}
			rec.EmbedModel = ix.kb.Model
		}
	} else if ix.kb.Embedder != nil && ix.kb.Model != "" {
		ix.embedRetryAt[f.rel] = ix.now().Add(ix.cfg.EmbedRetry)
	}
	if err := ix.kb.Store.Replace(ctx, rec, ec); err != nil {
		if ctx.Err() == nil {
			ix.log.Error("could not store a file", "path", f.rel, "err", err)
		}
		return outSkipped
	}
	if hadOld {
		return outChanged
	}
	ix.log.Info("file added to the knowledge base", "path", f.rel, "passages", len(chunks))
	return outAdded
}

// reembed embeds the stored passages of a file again (the embedding model was
// changed or switched off, or the earlier attempt failed). It reports success.
func (ix *Indexer) reembed(ctx context.Context, row File, embedDown *bool) bool {
	store := ix.kb.Store
	if ix.kb.Embedder == nil || ix.kb.Model == "" {
		if err := store.SetEmbeddings(ctx, row.ID, "", nil, nil); err != nil {
			ix.log.Error("could not clear embeddings", "path", row.Path, "err", err)
			return false
		}
		return true
	}
	if *embedDown {
		return false
	}
	stored, err := store.Chunks(ctx, row.ID)
	if err != nil || len(stored) == 0 {
		return false
	}
	chunks := make([]Chunk, len(stored))
	ids := make([]int64, len(stored))
	for i, c := range stored {
		chunks[i], ids[i] = c.Chunk, c.ID
	}
	vecs, err := ix.embed(ctx, chunks)
	if err != nil {
		if ctx.Err() == nil {
			*embedDown = true
			ix.cfg.Metrics.EmbedFailures.Inc()
			ix.embedRetryAt[row.Path] = ix.now().Add(ix.cfg.EmbedRetry)
			ix.log.Warn("could not embed a file; will try again later", "path", row.Path, "err", err)
		}
		return false
	}
	if err := store.SetEmbeddings(ctx, row.ID, ix.kb.Model, ids, vecs); err != nil {
		ix.log.Error("could not store embeddings", "path", row.Path, "err", err)
		return false
	}
	delete(ix.embedRetryAt, row.Path)
	ix.log.Info("passages embedded", "path", row.Path, "model", ix.kb.Model)
	return true
}

func (ix *Indexer) embed(ctx context.Context, chunks []Chunk) ([][]float32, error) {
	if ix.kb.Embedder == nil {
		return nil, errNoEmbedder
	}
	out := make([][]float32, 0, len(chunks))
	for i := 0; i < len(chunks); i += ix.cfg.EmbedBatch {
		end := min(i+ix.cfg.EmbedBatch, len(chunks))
		in := make([]string, end-i)
		for j := range in {
			in[j] = chunks[i+j].Text
		}
		vecs, err := ix.kb.Embedder.Embed(ctx, ix.kb.Model, in)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// readFile reads a file of the folder through the root, refusing more than max bytes.
func readFile(root *os.Root, rel string, max int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, attach.ErrTooLarge
	}
	return buf.Bytes(), nil
}
