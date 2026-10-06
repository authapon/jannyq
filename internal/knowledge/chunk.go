// Package knowledge is the shared knowledge base: the text and PDF files of a
// folder, split into passages, kept in SQLite with a full-text index and
// (optionally) embeddings, and searched with both. The index follows the
// folder: edits, deletions and renames of files update it.
package knowledge

import (
	"strings"
	"unicode"
)

// Chunk is one passage of a file.
type Chunk struct {
	Page int // 1-based page of a PDF; 0 for files without pages
	Text string
}

// minChunkChars drops passages that are only a few characters (page numbers,
// stray lines).
const minChunkChars = 12

// chunkPages splits the pages of a file into passages of about size characters,
// each starting where the previous one ended minus overlap, preferably cut at
// paragraph, line or sentence ends. With markdown, every passage is prefixed
// with the heading it sits under, so that it makes sense on its own.
func chunkPages(pages []string, size, overlap int, markdown, numbered bool) []Chunk {
	if size < 200 {
		size = 200
	}
	if overlap < 0 || overlap > size/2 {
		overlap = size / 8
	}
	var out []Chunk
	heading := ""
	for i, page := range pages {
		pageNo := 0
		if numbered {
			pageNo = i + 1
		}
		for _, text := range splitText(page, size, overlap) {
			t := text
			if markdown {
				startsWithHeading := headingOf(strings.SplitN(text, "\n", 2)[0]) != ""
				if heading != "" && !startsWithHeading {
					t = heading + "\n" + text
				}
				for _, line := range strings.Split(text, "\n") { // headings inside the passage apply to the next ones
					if h := headingOf(line); h != "" {
						heading = h
					}
				}
			}
			if nonBlankRunes(t) >= minChunkChars {
				out = append(out, Chunk{Page: pageNo, Text: t})
			}
		}
	}
	return out
}

// headingOf returns the markdown heading on line, or "".
func headingOf(line string) string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return ""
	}
	if rest := strings.TrimLeft(t, "#"); strings.HasPrefix(rest, " ") && strings.TrimSpace(rest) != "" {
		return t
	}
	return ""
}

func nonBlankRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// splitText cuts one page into passages.
func splitText(s string, size, overlap int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	runes := []rune(s)
	if len(runes) <= size {
		return []string{s}
	}
	var out []string
	start := 0
	for start < len(runes) {
		end := start + size
		if end >= len(runes) {
			out = append(out, strings.TrimSpace(string(runes[start:])))
			break
		}
		cut := breakPoint(runes, start, end)
		out = append(out, strings.TrimSpace(string(runes[start:cut])))
		next := cut - overlap
		if next <= start { // always make progress
			next = cut
		}
		// start the next passage at a word or line boundary when there is one nearby
		next = snapForward(runes, next, cut)
		start = next
	}
	return out
}

// breakPoint finds where to cut runes[start:end]: after a blank line, a line
// break or a sentence end in its second half, else after a space, else at end.
func breakPoint(r []rune, start, end int) int {
	lo := start + (end-start)/2
	for _, pass := range []func(i int) bool{
		func(i int) bool { return r[i] == '\n' && i > 0 && r[i-1] == '\n' },
		func(i int) bool { return r[i] == '\n' },
		func(i int) bool {
			return (r[i] == '.' || r[i] == '!' || r[i] == '?' || r[i] == '。' || r[i] == '！' || r[i] == '？') &&
				i+1 < len(r) && unicode.IsSpace(r[i+1])
		},
		func(i int) bool { return r[i] == ' ' || r[i] == '\t' },
	} {
		for i := end - 1; i >= lo; i-- {
			if pass(i) {
				return i + 1
			}
		}
	}
	return end
}

// snapForward moves a passage start to just after the next space or line break
// within a short distance, so that passages do not begin in the middle of a
// word. Text without spaces (Thai) keeps the exact position.
func snapForward(r []rune, pos, limit int) int {
	for i := pos; i < limit && i < pos+40; i++ {
		if r[i] == ' ' || r[i] == '\n' {
			return i + 1
		}
	}
	return pos
}
