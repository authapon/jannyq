package knowledge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

var bg = context.Background()

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *Store, path string, texts []string, vecs [][]float32) {
	t.Helper()
	var cs []EmbeddedChunk
	for i, tx := range texts {
		c := EmbeddedChunk{Chunk: Chunk{Text: tx}}
		if vecs != nil {
			c.Vec = vecs[i]
		}
		cs = append(cs, c)
	}
	if err := s.Replace(bg, File{Path: path, Size: 1, MtimeNS: 1, Hash: path, Kind: "text"}, cs); err != nil {
		t.Fatal(err)
	}
}

func paths(h []Hit) string {
	var p []string
	for _, x := range h {
		p = append(p, x.Path)
	}
	return strings.Join(p, ",")
}

func TestFullTextSearchAcrossLanguages(t *testing.T) {
	s := openStore(t)
	put(t, s, "en.txt", []string{"The marmalade project shipped on time. Budget used: 4200 euros.", "The zeppelin inspection is scheduled for Tuesday."}, nil)
	put(t, s, "th.txt", []string{"รายงานประจำไตรมาส โครงการมะม่วงเสร็จตามกำหนด งบประมาณที่ใช้ 4200 บาท"}, nil)
	for q, want := range map[string]string{
		"marmalade budget": "en.txt",
		"ZEPPELIN":         "en.txt",
		"4200":             "en.txt,th.txt",
		"มะม่วง":           "th.txt",
		"งบประมาณโครงการมะม่วงเท่าไหร่": "th.txt", // a Thai sentence has no spaces to split on
		"does anything mention submarine?": "",
	} {
		hits, err := s.Search(bg, q, SearchOptions{})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		got := paths(hits)
		if q == "4200" { // both files match; order is not the point
			if !strings.Contains(got, "en.txt") || !strings.Contains(got, "th.txt") {
				t.Errorf("%q: %s", q, got)
			}
			continue
		}
		if !strings.HasPrefix(got, want) || (want == "" && got != "") {
			t.Errorf("%q: got %q, want %q", q, got, want)
		}
	}
}

func TestShortWordsAndSyntaxAreSafe(t *testing.T) {
	s := openStore(t)
	put(t, s, "a.txt", []string{"Go is a language; the C++ and AT&T docs, 50% off, snake_case here"}, nil)
	for _, q := range []string{"go", "C++", "50%", `"unbalanced`, "a OR", "NEAR(", "snake_case", "*", "", "   ", "\x00", "col:umn", "AT&T"} {
		if _, err := s.Search(bg, q, SearchOptions{}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	if h, _ := s.Search(bg, "go", SearchOptions{}); len(h) != 1 {
		t.Errorf("a two-letter word is found by the fallback: %v", h)
	}
	if h, _ := s.Search(bg, "50%", SearchOptions{}); len(h) != 1 {
		t.Errorf("percent is not a wildcard but text: %v", h)
	}
	if h, _ := s.Search(bg, "xy", SearchOptions{}); len(h) != 0 {
		t.Errorf("no match expected: %v", h)
	}
}

func TestVectorSearchAndFusion(t *testing.T) {
	s := openStore(t)
	put(t, s, "cats.txt", []string{"Felines purr when content"}, [][]float32{{1, 0, 0}})
	put(t, s, "dogs.txt", []string{"Canines wag their tails"}, [][]float32{{0, 1, 0}})
	put(t, s, "mix.txt", []string{"Pets such as cats and dogs"}, [][]float32{{0.7, 0.7, 0}})
	// the query shares no word with any text: only vectors can find it
	hits, err := s.Search(bg, "kitten", SearchOptions{QueryVec: []float32{0.9, 0.1, 0}, MinCosine: 0.3})
	if err != nil || len(hits) == 0 || hits[0].Path != "cats.txt" || hits[0].ByWords || hits[0].Cosine < 0.9 {
		t.Fatalf("%v %+v", err, hits)
	}
	if hits, _ = s.Search(bg, "kitten", SearchOptions{QueryVec: []float32{0.9, 0.1, 0}, MinCosine: 0.999}); len(hits) != 0 {
		t.Errorf("the similarity floor is ignored: %+v", hits)
	}
	// a passage found both ways beats one found one way
	hits, _ = s.Search(bg, "dogs", SearchOptions{QueryVec: []float32{0.1, 0.9, 0}})
	// mix.txt has the word and a fair similarity; dogs.txt is the closest in meaning but lacks the word
	if len(hits) < 2 || hits[0].Path != "mix.txt" || hits[1].Path != "dogs.txt" {
		t.Fatalf("%+v", hits)
	}
	if !hits[0].ByWords || hits[0].Cosine == 0 {
		t.Errorf("fusion lost a signal: %+v", hits[0])
	}
	// words still work when the query could not be embedded
	if hits, _ = s.Search(bg, "tails", SearchOptions{}); len(hits) != 1 || hits[0].Path != "dogs.txt" {
		t.Errorf("%+v", hits)
	}
	// vectors of another size (another model) are ignored, not mixed in
	put(t, s, "old.txt", []string{"Legacy vector of another model"}, [][]float32{{1, 0}})
	hits, err = s.Search(bg, "zzzz", SearchOptions{QueryVec: []float32{1, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Path == "old.txt" {
			t.Error("a vector of the wrong dimension was compared")
		}
	}
	// the limit applies
	if hits, _ = s.Search(bg, "x", SearchOptions{Limit: 1, QueryVec: []float32{1, 1, 0}}); len(hits) != 1 {
		t.Errorf("limit: %d", len(hits))
	}
}

func TestReplaceRemoveAndRenameKeepTheIndexConsistent(t *testing.T) {
	s := openStore(t)
	put(t, s, "doc.txt", []string{"alpha one", "bravo two"}, [][]float32{{1, 0}, {0, 1}})
	if h, _ := s.Search(bg, "alpha", SearchOptions{}); len(h) != 1 {
		t.Fatalf("%v", h)
	}
	// editing replaces the old passages; the full-text index forgets the old words
	put(t, s, "doc.txt", []string{"charlie three"}, [][]float32{{0, 1}})
	if h, _ := s.Search(bg, "alpha", SearchOptions{}); len(h) != 0 {
		t.Errorf("old words are still found: %+v", h)
	}
	if h, _ := s.Search(bg, "charlie", SearchOptions{}); len(h) != 1 {
		t.Errorf("new words are not found: %+v", h)
	}
	st, _ := s.Stats(bg)
	if st.Files != 1 || st.Chunks != 1 || st.Embedded != 1 {
		t.Errorf("stats = %+v", st)
	}
	// vectors of replaced passages are gone from the cache too
	if h, _ := s.Search(bg, "", SearchOptions{QueryVec: []float32{1, 0}, MinCosine: 0.9}); len(h) != 0 {
		t.Errorf("a stale vector matched: %+v", h)
	}
	// rename keeps the passages
	if err := s.Rename(bg, "doc.txt", "new/name.txt", 5, 6); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Search(bg, "charlie", SearchOptions{}); len(h) != 1 || h[0].Path != "new/name.txt" {
		t.Errorf("after rename: %+v", h)
	}
	if err := s.Remove(bg, "new/name.txt"); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.Search(bg, "charlie", SearchOptions{QueryVec: []float32{0, 1}}); len(h) != 0 {
		t.Errorf("after remove: %+v", h)
	}
	if st, _ = s.Stats(bg); st.Files != 0 || st.Chunks != 0 {
		t.Errorf("stats = %+v", st)
	}
	if err := s.Remove(bg, "never-existed.txt"); err != nil {
		t.Errorf("removing an unknown file: %v", err)
	}
}

func TestSetEmbeddings(t *testing.T) {
	s := openStore(t)
	put(t, s, "a.txt", []string{"first passage text", "second passage text"}, nil)
	fs, _ := s.Files(bg)
	cs, _ := s.Chunks(bg, fs[0].ID)
	if err := s.SetEmbeddings(bg, fs[0].ID, "m1", []int64{cs[0].ID, cs[1].ID}, [][]float32{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(bg); st.Embedded != 2 {
		t.Errorf("%+v", st)
	}
	if fs, _ = s.Files(bg); fs[0].EmbedModel != "m1" {
		t.Errorf("model = %q", fs[0].EmbedModel)
	}
	if hits, _ := s.Search(bg, "", SearchOptions{QueryVec: []float32{1, 0}, MinCosine: 0.9}); len(hits) != 1 {
		t.Errorf("%+v", hits)
	}
	if err := s.SetEmbeddings(bg, fs[0].ID, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(bg); st.Embedded != 0 {
		t.Errorf("embeddings not cleared: %+v", st)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kb.db")
	s, _ := Open(path)
	put(t, s, "a.txt", []string{"persistent words"}, [][]float32{{1, 2, 3}})
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if h, _ := s.Search(bg, "persistent", SearchOptions{QueryVec: []float32{1, 2, 3}}); len(h) != 1 || h[0].Cosine < 0.99 {
		t.Errorf("%+v", h)
	}
}
