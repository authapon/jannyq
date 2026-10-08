package knowledge

import (
	"context"
	"fmt"
	"strings"
)

// FullText rebuilds the text of a file from its stored passages: the text that
// the next passage repeats from the previous one (the overlap, of about
// overlap characters as set for the chunker; 0 means passages do not overlap)
// and the heading
// line the chunker put in front of passages are taken out again, and the pages
// of a PDF are marked "[page N]". It returns the number of pages (0 for text
// files). The result is the file's text as it was read, not the file's bytes.
func (s *Store) FullText(ctx context.Context, path string, overlap int) (text string, pages int, err error) {
	var ps []Hit
	for from := 0; ; from += 500 {
		batch, err := s.Passages(ctx, path, from, 500)
		if err != nil {
			return "", 0, err
		}
		ps = append(ps, batch...)
		if len(batch) < 500 {
			break
		}
	}
	text, pages = joinPassages(ps, overlap)
	return text, pages, nil
}

// joinPassages joins passages in order (see FullText).
func joinPassages(ps []Hit, overlap int) (string, int) {
	// The chunker starts a passage "overlap" characters before the end of the
	// previous one, moved on by up to 40 to a word start: a repeated stretch of
	// less than a third of the overlap is a coincidence, not the overlap.
	minK := overlap / 3
	var sb strings.Builder
	var prevRaw, prevFull string // the previous passage as stored, and without its inherited heading
	page, pages := 0, 0
	for _, p := range ps {
		full := withoutInherited(prevRaw, p.Text)
		cur := full
		switch {
		case p.Page > 0 && p.Page != page:
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			fmt.Fprintf(&sb, "[page %d]\n", p.Page)
			page = p.Page
			pages++
		case sb.Len() > 0:
			if k := overlapLen(prevFull, full, minK, overlap); k > 0 {
				cur = string([]rune(full)[k:])
			} else {
				sb.WriteString("\n")
			}
		}
		sb.WriteString(cur)
		prevRaw, prevFull = p.Text, full
	}
	return sb.String(), pages
}

// overlapLen returns how many runes at the start of b repeat the end of a, at
// least minK and at most maxK of them (the chunker never repeats more than the
// overlap it was set to, which keeps repetitive text from matching too much),
// or 0.
func overlapLen(a, b string, minK, maxK int) int {
	if minK < 1 {
		return 0 // no overlap is expected
	}
	ar, br := []rune(a), []rune(b)
	maxK = min(maxK, len(ar), len(br))
	for k := maxK; k >= minK; k-- {
		if string(ar[len(ar)-k:]) == string(br[:k]) {
			return k
		}
	}
	return 0
}
