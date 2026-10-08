package knowledge

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const schemaVersion = 2

const schema = `
CREATE TABLE IF NOT EXISTS files (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	path        TEXT NOT NULL UNIQUE,
	size        INTEGER NOT NULL,
	mtime_ns    INTEGER NOT NULL,
	hash        TEXT NOT NULL DEFAULT '',
	kind        TEXT NOT NULL DEFAULT '',
	pages       INTEGER NOT NULL DEFAULT 0,
	chunks      INTEGER NOT NULL DEFAULT 0,
	status      TEXT NOT NULL DEFAULT 'ok',
	error       TEXT NOT NULL DEFAULT '',
	embed_model TEXT NOT NULL DEFAULT '',
	indexed_at  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS chunks (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	file_id   INTEGER NOT NULL,
	ord       INTEGER NOT NULL,
	page      INTEGER NOT NULL DEFAULT 0,
	text      TEXT NOT NULL,
	embedding BLOB
);
CREATE INDEX IF NOT EXISTS chunks_file ON chunks(file_id, ord);
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(text, content='chunks', content_rowid='id', tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
	INSERT INTO chunks_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
	INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
`

// Store is the knowledge database. It is safe for concurrent use: one
// goroutine indexes while others search.
type Store struct {
	db *sql.DB

	mu   sync.RWMutex
	vecs []vector // cache of all embeddings; nil when it has to be reloaded
	live bool
}

type vector struct {
	id int64
	v  []float32 // unit length
}

// File is a source file as the index knows it.
type File struct {
	ID         int64
	Path       string // relative to the knowledge folder, with forward slashes
	Size       int64
	MtimeNS    int64
	Hash       string
	Kind       string // "pdf" or "text"
	Pages      int
	Chunks     int
	Status     string // "ok" or "error"
	Error      string
	EmbedModel string
	IndexedAt  time.Time
}

// Open opens (creating it if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init knowledge db: %w", err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version == 1 {
		// Version 1 wrongly refused some valid UTF-8 text files as binary data. Files recorded as failed
		// are read again once: an impossible size makes the next scan treat them as changed.
		if _, err := db.Exec(`UPDATE files SET size = -1, mtime_ns = 0 WHERE status != 'ok'`); err != nil {
			db.Close()
			return nil, err
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) invalidate() {
	s.mu.Lock()
	s.vecs, s.live = nil, false
	s.mu.Unlock()
}

const fileColumns = `id, path, size, mtime_ns, hash, kind, pages, chunks, status, error, embed_model, indexed_at`

func scanFile(row interface{ Scan(...any) error }) (File, error) {
	var f File
	var at int64
	err := row.Scan(&f.ID, &f.Path, &f.Size, &f.MtimeNS, &f.Hash, &f.Kind, &f.Pages, &f.Chunks, &f.Status, &f.Error, &f.EmbedModel, &at)
	f.IndexedAt = time.Unix(at, 0)
	return f, err
}

// Files lists every known file by path.
func (s *Store) Files(ctx context.Context) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+fileColumns+` FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Stats summarises the index.
type Stats struct {
	Files, Failed, Chunks, Embedded int
}

// Stats counts files, failed files, passages and passages with embeddings.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM files WHERE status = 'ok'),
		(SELECT COUNT(*) FROM files WHERE status != 'ok'),
		(SELECT COUNT(*) FROM chunks),
		(SELECT COUNT(*) FROM chunks WHERE embedding IS NOT NULL)`).Scan(&st.Files, &st.Failed, &st.Chunks, &st.Embedded)
	return st, err
}

// EmbeddedChunk is a passage handed to Replace with its vector (nil if none).
type EmbeddedChunk struct {
	Chunk
	Vec []float32
}

// Replace stores the passages of a file, replacing whatever the file had.
func (s *Store) Replace(ctx context.Context, f File, chunks []EmbeddedChunk) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := upsertFile(ctx, tx, f, len(chunks))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE file_id = ?`, id); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO chunks(file_id, ord, page, text, embedding) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, c := range chunks {
		if _, err := stmt.ExecContext(ctx, id, i, c.Page, c.Text, encodeVec(c.Vec)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func upsertFile(ctx context.Context, tx *sql.Tx, f File, chunks int) (int64, error) {
	if f.Status == "" {
		f.Status = "ok"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO files(path, size, mtime_ns, hash, kind, pages, chunks, status, error, embed_model, indexed_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, hash=excluded.hash, kind=excluded.kind,
			pages=excluded.pages, chunks=excluded.chunks, status=excluded.status, error=excluded.error,
			embed_model=excluded.embed_model, indexed_at=excluded.indexed_at`,
		f.Path, f.Size, f.MtimeNS, f.Hash, f.Kind, f.Pages, chunks, f.Status, f.Error, f.EmbedModel, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE path = ?`, f.Path).Scan(&id)
	return id, err
}

// Touch updates the size and time of a file whose content did not change.
func (s *Store) Touch(ctx context.Context, path string, size, mtimeNS int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET size = ?, mtime_ns = ? WHERE path = ?`, size, mtimeNS, path)
	return err
}

// Rename moves a file's passages to a new path (the file was renamed, not edited).
func (s *Store) Rename(ctx context.Context, oldPath, newPath string, size, mtimeNS int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET path = ?, size = ?, mtime_ns = ? WHERE path = ?`, newPath, size, mtimeNS, oldPath)
	return err
}

// Remove forgets a file and its passages.
func (s *Store) Remove(ctx context.Context, path string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE file_id = (SELECT id FROM files WHERE path = ?)`, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

// StoredChunk is a passage as stored.
type StoredChunk struct {
	ID int64
	Chunk
}

// Chunks returns the passages of a file in order.
func (s *Store) Chunks(ctx context.Context, fileID int64) ([]StoredChunk, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, page, text FROM chunks WHERE file_id = ? ORDER BY ord`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredChunk
	for rows.Next() {
		var c StoredChunk
		if err := rows.Scan(&c.ID, &c.Page, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetEmbeddings stores vectors for passages of a file and records the model.
// A nil vecs clears them (embedding was switched off).
func (s *Store) SetEmbeddings(ctx context.Context, fileID int64, model string, ids []int64, vecs [][]float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if vecs == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedding = NULL WHERE file_id = ?`, fileID); err != nil {
			return err
		}
	}
	for i, v := range vecs {
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET embedding = ? WHERE id = ?`, encodeVec(v), ids[i]); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET embed_model = ? WHERE id = ?`, model, fileID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidate()
	return nil
}

func encodeVec(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return nil
	}
	n := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = f * n
	}
	return out
}

// loadVectors returns the cached embeddings, reading them from the database
// when the index changed since they were last loaded.
func (s *Store) loadVectors(ctx context.Context) ([]vector, error) {
	s.mu.RLock()
	if s.live {
		v := s.vecs
		s.mu.RUnlock()
		return v, nil
	}
	s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `SELECT id, embedding FROM chunks WHERE embedding IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vecs []vector
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		if v := normalize(decodeVec(b)); v != nil {
			vecs = append(vecs, vector{id, v})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.vecs, s.live = vecs, true
	s.mu.Unlock()
	return vecs, nil
}

// Hit is a passage found by Search.
type Hit struct {
	ChunkID int64
	Path    string
	Kind    string
	Page    int
	Ord     int
	Text    string
	Score   float64 // fused rank score; higher is better
	Cosine  float64 // similarity to the query when it was embedded; 0 otherwise
	ByWords bool    // matched the words of the query
}

// SearchOptions tunes Search.
type SearchOptions struct {
	Limit int
	// QueryVec is the embedded query; nil searches by words only.
	QueryVec []float32
	// MinCosine drops vector matches less similar than this to the query.
	MinCosine float64
}

const rrfK = 60

// Search finds the passages that best answer query: the full-text matches and
// the nearest embeddings are merged by reciprocal rank fusion.
func (s *Store) Search(ctx context.Context, query string, o SearchOptions) ([]Hit, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 5
	}
	pool := max(limit*4, 20)

	ftsIDs, err := s.searchText(ctx, query, pool)
	if err != nil {
		return nil, err
	}
	score := map[int64]float64{}
	textHit := map[int64]bool{}
	cos := map[int64]float64{}
	for rank, id := range ftsIDs {
		score[id] += 1 / float64(rrfK+rank+1)
		textHit[id] = true
	}
	if q := normalize(o.QueryVec); q != nil {
		vecs, err := s.loadVectors(ctx)
		if err != nil {
			return nil, err
		}
		type scored struct {
			id  int64
			sim float64
		}
		var best []scored
		for _, v := range vecs {
			if len(v.v) != len(q) {
				continue // embedded with another model
			}
			var dot float32
			for i := range q {
				dot += q[i] * v.v[i]
			}
			if float64(dot) >= o.MinCosine {
				best = append(best, scored{v.id, float64(dot)})
			}
		}
		sort.Slice(best, func(i, j int) bool { return best[i].sim > best[j].sim })
		if len(best) > pool {
			best = best[:pool]
		}
		for rank, b := range best {
			score[b.id] += 1 / float64(rrfK+rank+1)
			cos[b.id] = b.sim
		}
	}
	if len(score) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}
	hits, err := s.loadHits(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range hits {
		hits[i].Score = score[hits[i].ChunkID]
		hits[i].Cosine = cos[hits[i].ChunkID]
		hits[i].ByWords = textHit[hits[i].ChunkID]
	}
	return hits, nil
}

func (s *Store) loadHits(ctx context.Context, ids []int64) ([]Hit, error) {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, f.path, f.kind, c.page, c.ord, c.text
		FROM chunks c JOIN files f ON f.id = c.file_id WHERE c.id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[int64]Hit{}
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.Path, &h.Kind, &h.Page, &h.Ord, &h.Text); err != nil {
			return nil, err
		}
		byID[h.ChunkID] = h
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(ids))
	for _, id := range ids { // keep the ranking
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// searchText returns the ids of passages matching the words of query, best first.
func (s *Store) searchText(ctx context.Context, query string, limit int) ([]int64, error) {
	terms, short := queryTerms(query)
	var ids []int64
	if len(terms) > 0 {
		match := make([]string, len(terms))
		for i, t := range terms {
			match[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT rowid FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY bm25(chunks_fts) LIMIT ?`,
			strings.Join(match, " OR "), limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(terms) == 0 && len(short) > 0 {
		// the trigram index needs three characters: look for shorter words directly
		like := "%" + escapeLike(short[0]) + "%"
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM chunks WHERE text LIKE ? ESCAPE '\' LIMIT ?`, like, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
	return ids, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// queryTerms turns a question into search terms for the trigram index: words
// of three or more characters, and for text without spaces (Thai, Chinese,
// Japanese) overlapping fragments of long words. Words shorter than three
// characters cannot be looked up in the index and are returned in short.
func queryTerms(q string) (terms, short []string) {
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || r == '_' || r == '-' || r == '.')
	}) {
		w = strings.Trim(w, "-._")
		n := len([]rune(w))
		switch {
		case n == 0:
		case n < 3:
			short = append(short, w)
		case n > 7 && mostlyUnspaced(w):
			for _, f := range fragments(w, 4, 2) {
				add(f)
			}
		default:
			add(w)
		}
	}
	const maxTerms = 24
	if len(terms) > maxTerms {
		terms = terms[:maxTerms]
	}
	return terms, short
}

// mostlyUnspaced reports whether most letters of w belong to scripts written
// without spaces between words.
func mostlyUnspaced(w string) bool {
	n, u := 0, 0
	for _, r := range w {
		n++
		if unicode.Is(unicode.Thai, r) || unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Lao, r) || unicode.Is(unicode.Khmer, r) || unicode.Is(unicode.Myanmar, r) {
			u++
		}
	}
	return n > 0 && u*2 > n
}

func fragments(w string, size, stride int) []string {
	r := []rune(w)
	var out []string
	for i := 0; i+size <= len(r); i += stride {
		out = append(out, string(r[i:i+size]))
	}
	if tail := string(r[max(len(r)-size, 0):]); len(out) == 0 || out[len(out)-1] != tail {
		out = append(out, tail)
	}
	return out
}
