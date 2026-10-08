package knowledge

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func storeChunks(t *testing.T, path string, chunks []Chunk) *Store {
	t.Helper()
	s := openStore(t)
	ec := make([]EmbeddedChunk, len(chunks))
	for i, c := range chunks {
		ec[i].Chunk = c
	}
	if err := s.Replace(bg, File{Path: path, Size: 1, MtimeNS: 1, Kind: "text"}, ec); err != nil {
		t.Fatal(err)
	}
	return s
}

// norm compares texts without caring about runs of white space.
func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestFullTextRoundTrip(t *testing.T) {
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "ไทย", "ภาษาไทยไม่มีช่องว่างระหว่างคำ", "1.5.3", "PLO1", "x"}
	rng := rand.New(rand.NewSource(7))
	para := func(n int) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteString(words[rng.Intn(len(words))])
			sb.WriteString(" ")
		}
		return strings.TrimSpace(sb.String())
	}
	for round := 0; round < 40; round++ {
		var doc strings.Builder
		if round%2 == 0 {
			doc.WriteString("# Title\n\n")
		}
		for i := 0; i < 5+rng.Intn(20); i++ {
			if round%2 == 0 && i%4 == 0 {
				fmt.Fprintf(&doc, "## Section %d\n", i)
			}
			doc.WriteString(para(5+rng.Intn(60)) + "\n\n")
		}
		text := doc.String()
		size, overlap := 400+rng.Intn(400), 100+rng.Intn(60)
		chunks := chunkPages([]string{text}, size, overlap, round%2 == 0, false)
		s := storeChunks(t, "f.md", chunks)
		got, pages, err := s.FullText(bg, "f.md", overlap)
		if err != nil || pages != 0 {
			t.Fatalf("round %d: %v pages %d", round, err, pages)
		}
		if w, g := strings.Fields(text), strings.Fields(got); strings.Join(w, " ") != strings.Join(g, " ") {
			i := 0
			for i < len(w) && i < len(g) && w[i] == g[i] {
				i++
			}
			t.Fatalf("round %d (size %d, overlap %d): the rebuilt text differs from word %d\nwant …%q\ngot  …%q", round, size, overlap, i,
				strings.Join(w[max(i-6, 0):min(i+6, len(w))], " "), strings.Join(g[max(i-6, 0):min(i+6, len(g))], " "))
		}
	}
}

func TestFullTextMarksPages(t *testing.T) {
	numbered := func(tag string) string {
		var sb strings.Builder
		for i := 0; i < 30; i++ {
			fmt.Fprintf(&sb, "%s sentence %d goes here. ", tag, i)
		}
		return sb.String()
	}
	page1, page2 := numbered("first page"), numbered("second page")
	var chunks []Chunk
	for n, pg := range []string{page1, page2} {
		for _, c := range chunkPages([]string{pg}, 300, 40, false, false) {
			c.Page = n + 1
			chunks = append(chunks, c)
		}
	}
	s := storeChunks(t, "d.pdf", chunks)
	got, pages, err := s.FullText(bg, "d.pdf", 40)
	if err != nil || pages != 2 {
		t.Fatalf("%v pages %d", err, pages)
	}
	if !strings.HasPrefix(got, "[page 1]\nfirst page sentence 0") || !strings.Contains(got, "\n\n[page 2]\nsecond page sentence 0") {
		t.Errorf("page marks missing:\n%.200s", got)
	}
	if norm(strings.ReplaceAll(strings.ReplaceAll(got, "[page 1]", ""), "[page 2]", "")) != norm(page1+" "+page2) {
		t.Error("the text of the pages is not complete and exact")
	}
}

func TestFullTextOfAMissingFile(t *testing.T) {
	s := openStore(t)
	if _, _, err := s.FullText(bg, "nope.md", 40); err == nil {
		t.Error("an unknown file must be an error")
	}
}

func TestOverlapLen(t *testing.T) {
	if k := overlapLen("abc defghijklmnop qrstuvwxyz", "qrstuvwxyz and more", 5, 40); k != len("qrstuvwxyz") {
		t.Errorf("k = %d", k)
	}
	if k := overlapLen("one two three", "unrelated text here", 5, 40); k != 0 {
		t.Errorf("no overlap: %d", k)
	}
	if k := overlapLen("short ab", "ab then", 5, 40); k != 0 {
		t.Errorf("a coincidence shorter than the minimum is not an overlap: %d", k)
	}
	if k := overlapLen("abcdefghij0123456789", "0123456789xyz", 3, 6); k != 0 {
		t.Errorf("more than the overlap that was set is never taken: %d", k)
	}
	if k := overlapLen("the end", "the end of it", 0, 0); k != 0 {
		t.Errorf("passages that do not overlap are never trimmed: %d", k)
	}
}
