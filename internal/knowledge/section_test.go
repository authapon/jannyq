package knowledge

import (
	"fmt"
	"strings"
	"testing"
)

func TestHeadingLevel(t *testing.T) {
	for line, want := range map[string]int{
		"# Title": 1,
		"## หมวดที่ 2 ข้อมูลเฉพาะ": 2,
		"###### deep": 6,
		"1.6 รายละเอียดผลลัพธ์":    12,
		"1.5.3 ผลลัพธ์การเรียนรู้": 13,
		"2.1) Admission rules":            12,
		"1.5.3.1.2 very deep":             15,
		"1. a list item":                  0,
		"2) another list item":            0,
		"1 alone":                         0,
		"PLO1 แก้ไขปัญหา":                 0,
		"| 1.2 | table row |":             0,
		"#hashtag":                        0,
		"####### seven":                   0,
		"":                                0,
		"   ":                             0,
		"1.5 " + strings.Repeat("x", 200): 0, // a long line is text
	} {
		if got := headingLevel(line); got != want {
			t.Errorf("headingLevel(%q) = %d, want %d", line, got, want)
		}
	}
}

func TestWithoutInherited(t *testing.T) {
	prev := "text\n## Chapter 1\nmore text"
	if got := withoutInherited(prev, "## Chapter 1\nbody"); got != "body" {
		t.Errorf("inherited heading kept: %q", got)
	}
	if got := withoutInherited(prev, "## Chapter 2\nbody"); got != "## Chapter 2\nbody" {
		t.Errorf("a heading of its own was removed: %q", got)
	}
	if got := withoutInherited(prev, "1.5.3 numbered\nbody"); got != "1.5.3 numbered\nbody" {
		t.Errorf("numbered headings are never inherited: %q", got)
	}
}

// curriculum builds a file like the real one: a Markdown chapter heading, then
// numbered sections, cut into small passages so that sections span several.
func curriculum(t *testing.T) (*Store, []Chunk) {
	t.Helper()
	filler := func(tag string, n int) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "%s sentence %d about something. ", tag, i)
		}
		return sb.String()
	}
	var lb strings.Builder
	for i, w := range []string{"maths", "economics", "programs", "systems", "analysis", "communication", "research", "ethics", "work placement"} {
		fmt.Fprintf(&lb, "PLO%d %s, described at some length so that the list is long enough to span passages.\n", i+1, w)
	}
	list := lb.String()
	page := "## Chapter 1 General\n" +
		"1.4 Philosophy\n" + filler("philosophy", 30) + "\n" +
		"1.5 Objectives\n" + filler("objectives", 12) + "\n" +
		"1.5.3 Program Learning Outcomes (PLOs)\nAfter graduation the learner can\n" + list + "\n" +
		"1.6 Details of outcomes\n" + filler("details", 30) + "\n" +
		"1.7 Other\n" + filler("other", 30) + "\n"
	chunks := chunkPages([]string{page}, 500, 40, true, false)
	s := openStore(t)
	ec := make([]EmbeddedChunk, len(chunks))
	for i, c := range chunks {
		ec[i].Chunk = c
	}
	if err := s.Replace(bg, File{Path: "c.md", Size: 1, MtimeNS: 1, Kind: "text"}, ec); err != nil {
		t.Fatal(err)
	}
	return s, chunks
}

func indexOf(chunks []Chunk, needle string) int {
	for i, c := range chunks {
		if strings.Contains(c.Text, needle) {
			return i
		}
	}
	return -1
}

func texts(hs []Hit) string {
	var sb strings.Builder
	for _, h := range hs {
		sb.WriteString(h.Text + "\n")
	}
	return sb.String()
}

func TestContextCompletesASectionThatSpansPassages(t *testing.T) {
	s, chunks := curriculum(t)
	first, last := indexOf(chunks, "PLO1 maths,"), indexOf(chunks, "PLO9 work placement,")
	if first == last {
		t.Fatalf("the list must span passages for this test (passages %d..%d)", first, last)
	}

	// a hit on the opening passage is followed to the end of its section...
	before, after, err := s.Context(bg, "c.md", first, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if all := chunks[first].Text + texts(after); !strings.Contains(all, "PLO9 work placement,") {
		t.Errorf("the rest of the list is missing:\n%s", texts(after))
	}
	// ...and not beyond: the passage in which the next section opens is the last
	if got := after[len(after)-1].Ord; got != indexOf(chunks, "1.6 Details") {
		t.Errorf("stopped at passage %d, section 1.6 opens in %d", got, indexOf(chunks, "1.6 Details"))
	}
	if strings.Contains(texts(after), "other sentence") {
		t.Errorf("section 1.7 was read:\n%s", texts(after))
	}

	// a hit in the middle of the list is completed in both directions
	mid := indexOf(chunks, "PLO5 analysis,")
	before, after, _ = s.Context(bg, "c.md", mid, 100000)
	whole := texts(before) + chunks[mid].Text + texts(after)
	for _, want := range []string{"1.5.3 Program Learning Outcomes", "PLO1 maths,", "PLO9 work placement,"} {
		if !strings.Contains(whole, want) {
			t.Errorf("around passage %d: %q is missing:\n%s", mid, want, whole)
		}
	}
	if strings.Contains(whole, "objectives sentence 0") {
		t.Errorf("the previous section was read:\n%s", whole)
	}

	// a hit on the tail finds where the section opened
	before, _, _ = s.Context(bg, "c.md", last, 100000)
	if !strings.Contains(texts(before), "PLO1 maths,") {
		t.Errorf("the start of the list is missing:\n%s", texts(before))
	}
}

func TestContextRespectsTheBudgetAndFollowsFirst(t *testing.T) {
	s, chunks := curriculum(t)
	mid := indexOf(chunks, "PLO5 analysis,")
	b0, a0, _ := s.Context(bg, "c.md", mid, 0)
	if len(b0)+len(a0) != 0 {
		t.Error("a zero budget adds nothing")
	}
	b1, a1, _ := s.Context(bg, "c.md", mid, 1) // room for one passage only
	if len(a1) != 1 || len(b1) != 0 {
		t.Errorf("with room for one passage the following one comes first: before %d after %d", len(b1), len(a1))
	}
}

func TestContextWithoutHeadingsIsTheNextPassage(t *testing.T) {
	s := openStore(t)
	var ec []EmbeddedChunk
	for i := 0; i < 6; i++ {
		ec = append(ec, EmbeddedChunk{Chunk: Chunk{Text: fmt.Sprintf("plain passage number %d with no headings at all, just text", i)}})
	}
	if err := s.Replace(bg, File{Path: "p.txt", Size: 1, MtimeNS: 1, Kind: "text"}, ec); err != nil {
		t.Fatal(err)
	}
	before, after, err := s.Context(bg, "p.txt", 2, 100000)
	if err != nil || len(before) != 0 || len(after) != 1 || after[0].Ord != 3 {
		t.Errorf("before %d after %v err %v", len(before), after, err)
	}
	// nothing follows the last passage; an unknown file is an error from Passages, not a crash
	if b, a, err := s.Context(bg, "p.txt", 5, 100000); err != nil || len(b)+len(a) != 0 {
		t.Errorf("last passage: %d %d %v", len(b), len(a), err)
	}
	if b, a, err := s.Context(bg, "missing.txt", 0, 100); err == nil || len(b)+len(a) != 0 {
		t.Errorf("unknown file: %v", err)
	}
}

func TestContextOfASectionThatNeverEndsIsBounded(t *testing.T) {
	s := openStore(t)
	var ec []EmbeddedChunk
	ec = append(ec, EmbeddedChunk{Chunk: Chunk{Text: "1.1 One long section\nstart of it, which goes on"}})
	for i := 0; i < 30; i++ {
		ec = append(ec, EmbeddedChunk{Chunk: Chunk{Text: fmt.Sprintf("continuing text of the long section, part %d, without any new heading", i)}})
	}
	if err := s.Replace(bg, File{Path: "l.txt", Size: 1, MtimeNS: 1, Kind: "text"}, ec); err != nil {
		t.Fatal(err)
	}
	_, after, _ := s.Context(bg, "l.txt", 0, 1000000)
	if len(after) != maxForward {
		t.Errorf("%d passages followed, the limit is %d", len(after), maxForward)
	}
}
