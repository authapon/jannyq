package knowledge

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
)

// A document is read in sections, not in passages of a fixed size: a list, a
// table or a chapter runs on across the boundary between two passages. These
// helpers find where a section opens and closes from the headings in the text,
// so that a hit can be shown together with the rest of its section.

// numbered matches section headings such as "1.5.3 Learning outcomes" or
// "2.1) Admission": at least two levels, so that "1." or "2)" list items and
// lone numbers are not taken for headings.
var numbered = regexp.MustCompile(`^(\d{1,2}(?:\.\d{1,2}){1,4})[.)]?\s+\S`)

const maxHeadingChars = 160

// headingLevel returns how high a line ranks as a heading, 0 for text. Markdown
// headings rank by the number of marks (1 is highest); numbered headings rank
// below all of those, deeper numbers lower.
func headingLevel(line string) int {
	t := strings.TrimSpace(line)
	if t == "" || utf8.RuneCountInString(t) > maxHeadingChars {
		return 0
	}
	if strings.HasPrefix(t, "#") {
		rest := strings.TrimLeft(t, "#")
		if n := len(t) - len(rest); n <= 6 && strings.HasPrefix(rest, " ") && strings.TrimSpace(rest) != "" {
			return n
		}
		return 0
	}
	if m := numbered.FindStringSubmatch(t); m != nil {
		return 10 + strings.Count(m[1], ".") + 1
	}
	return 0
}

// headingsOf lists the levels of the headings in text, in order.
func headingsOf(text string) []int {
	var out []int
	for _, line := range strings.Split(text, "\n") {
		if l := headingLevel(line); l > 0 {
			out = append(out, l)
		}
	}
	return out
}

// openedHere reports whether the text starts a section of its own: its first
// line is a heading. A passage that starts with other text belongs, at least at
// its start, to a section opened earlier.
func openedHere(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return headingLevel(line) > 0
	}
	return false
}

// withoutInherited removes the line that the chunker put in front of a passage
// to say which heading it sits under: a Markdown heading that is the last
// heading of the passage before. What is left is the passage's own text.
func withoutInherited(prev, cur string) string {
	first, rest, ok := strings.Cut(cur, "\n")
	if !ok || headingLevel(first) == 0 || !strings.HasPrefix(strings.TrimSpace(first), "#") {
		return cur
	}
	last := ""
	for _, line := range strings.Split(prev, "\n") {
		if t := strings.TrimSpace(line); headingLevel(t) > 0 && strings.HasPrefix(t, "#") {
			last = t
		}
	}
	if last != "" && strings.TrimSpace(first) == last {
		return rest
	}
	return cur
}

const (
	maxBack    = 3 // passages looked at before a hit to find where its section opens
	maxForward = 5 // passages followed after a hit to find where it closes
)

// Context returns the passages around hit number ord of a file that belong to
// the same section: those back to the passage in which the section opens, and
// those on to the passage in which the next section of the same or a higher
// rank begins. Only about budget characters are returned (the following
// passages first, then the preceding ones nearest to the hit); a hit whose
// section cannot be told, because the text has no headings, gets just the
// passage that follows. budget <= 0 returns nothing.
func (s *Store) Context(ctx context.Context, path string, ord, budget int) (before, after []Hit, err error) {
	if budget <= 0 {
		return nil, nil, nil
	}
	lo := max(ord-maxBack-1, 0) // one more than needed, to tell what the first one inherits
	ps, err := s.Passages(ctx, path, lo, ord-lo+maxForward+1)
	if err != nil || len(ps) == 0 {
		return nil, nil, err
	}
	idx := ord - lo
	if idx >= len(ps) {
		return nil, nil, nil
	}
	text := make([]string, len(ps)) // each passage's own text
	for i := range ps {
		text[i] = ps[i].Text
		if i > 0 {
			text[i] = withoutInherited(ps[i-1].Text, ps[i].Text)
		}
	}
	first := 0 // the passage before the window is only there to explain the next
	if lo > 0 {
		first = 1
	}

	level := 0 // rank of the section the hit ends in
	if hs := headingsOf(text[idx]); len(hs) > 0 {
		level = hs[len(hs)-1]
	}
	start := idx
	if !openedHere(text[idx]) {
		found := false
		for k := idx - 1; k >= first; k-- {
			if hs := headingsOf(text[k]); len(hs) > 0 {
				if level == 0 {
					level = hs[len(hs)-1]
				}
				start, found = k, true
				break
			}
		}
		if !found {
			start = idx // the opening is too far back to say
		}
	}

	end := idx // last passage after the hit that is included
	switch {
	case level == 0:
		if idx+1 < len(ps) {
			end = idx + 1
		}
	default:
		for j := idx + 1; j < len(ps); j++ {
			end = j
			closes := false
			for _, l := range headingsOf(text[j]) {
				if l <= level {
					closes = true
				}
			}
			if closes {
				break
			}
		}
	}

	left := budget
	for j := idx + 1; j <= end && left > 0; j++ {
		after = append(after, ps[j])
		left -= utf8.RuneCountInString(ps[j].Text)
	}
	for k := idx - 1; k >= start && k >= first && left > 0; k-- {
		before = append([]Hit{ps[k]}, before...)
		left -= utf8.RuneCountInString(ps[k].Text)
	}
	return before, after, nil
}
