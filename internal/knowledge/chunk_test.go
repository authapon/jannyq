package knowledge

import (
	"fmt"
	"strings"
	"testing"
)

func TestChunkingKeepsEverythingAndOverlaps(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "Sentence number %d is here. ", i)
	}
	text := sb.String()
	chunks := chunkPages([]string{text}, 500, 80, false, false)
	if len(chunks) < 8 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c.Text)); n > 500 {
			t.Errorf("chunk %d has %d characters", i, n)
		}
		if c.Page != 0 {
			t.Errorf("page = %d", c.Page)
		}
	}
	// every sentence is fully present in some chunk
	for i := 0; i < 200; i++ {
		s := fmt.Sprintf("Sentence number %d is here.", i)
		found := false
		for _, c := range chunks {
			if strings.Contains(c.Text, s) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%q is split between chunks and found in none", s)
		}
	}
	// neighbouring chunks overlap
	last := chunks[0].Text[len(chunks[0].Text)-20:]
	if !strings.Contains(chunks[1].Text, strings.TrimSpace(last[strings.Index(last, " ")+1:])) {
		t.Errorf("no overlap between %q and %q", chunks[0].Text[len(chunks[0].Text)-40:], chunks[1].Text[:60])
	}
}

func TestChunkingThaiWithoutSpaces(t *testing.T) {
	text := strings.Repeat("โครงการมะม่วงเสร็จตามกำหนดและใช้งบประมาณตามที่ตั้งไว้", 60)
	chunks := chunkPages([]string{text}, 400, 50, false, false)
	if len(chunks) < 5 {
		t.Fatalf("%d chunks", len(chunks))
	}
	total := 0
	for _, c := range chunks {
		if n := len([]rune(c.Text)); n > 400 {
			t.Errorf("chunk of %d characters", n)
		}
		total += len([]rune(c.Text))
	}
	if total < len([]rune(text)) {
		t.Errorf("text was lost: %d of %d", total, len([]rune(text)))
	}
}

func TestPagesAreNumbered(t *testing.T) {
	chunks := chunkPages([]string{"First page text here.", "", "Third page has more words."}, 500, 50, false, true)
	if len(chunks) != 2 || chunks[0].Page != 1 || chunks[1].Page != 3 {
		t.Errorf("chunks = %+v", chunks)
	}
}

func TestTinyPassagesAreDropped(t *testing.T) {
	if got := chunkPages([]string{"12", " \n "}, 500, 50, false, true); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestMarkdownHeadingsTravelWithTheirPassages(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# Handbook\n\n## Vacation\n\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "Employees get %d days of leave after year %d of service.\n\n", i, i)
	}
	sb.WriteString("## Expenses\n\nReceipts are required for every expense.\n")
	chunks := chunkPages([]string{sb.String()}, 400, 40, true, false)
	var vac, exp int
	for _, c := range chunks {
		if strings.Contains(c.Text, "days of leave") && strings.Contains(c.Text, "## Vacation") {
			vac++
		}
		if strings.Contains(c.Text, "Receipts are required") && strings.Contains(c.Text, "## Expenses") {
			exp++
		}
		if strings.HasPrefix(c.Text, "## Expenses") && strings.Contains(c.Text, "days of leave") {
			t.Errorf("a vacation passage sits under the wrong heading: %q", c.Text)
		}
	}
	if vac < len(chunks)-2 || exp != 1 {
		t.Errorf("vacation passages with their heading: %d of %d, expenses: %d", vac, len(chunks), exp)
	}
	// without markdown nothing is added
	for _, c := range chunkPages([]string{sb.String()}, 400, 40, false, false) {
		if strings.HasPrefix(c.Text, "## Vacation\n## Vacation") {
			t.Error("heading duplicated")
		}
	}
}

func TestQueryTerms(t *testing.T) {
	terms, short := queryTerms(`How much is the "marmalade" budget? 4200 EUR`)
	if strings.Join(terms, ",") != "how,much,the,marmalade,budget,4200,eur" || len(short) != 1 || short[0] != "is" {
		t.Errorf("terms = %v short = %v", terms, short)
	}
	terms, _ = queryTerms("งบประมาณโครงการมะม่วงเท่าไหร่")
	if len(terms) < 4 {
		t.Fatalf("thai terms = %v", terms)
	}
	for _, f := range terms {
		if len([]rune(f)) < 3 {
			t.Errorf("fragment %q is too short for the trigram index", f)
		}
	}
	// quotes and operators of the full-text syntax are plain words here
	terms, _ = queryTerms(`a OR b" NEAR(x) *`)
	for _, f := range terms {
		if strings.ContainsAny(f, `"()*`) {
			t.Errorf("term %q carries syntax", f)
		}
	}
	if terms, short = queryTerms("   "); len(terms)+len(short) != 0 {
		t.Errorf("%v %v", terms, short)
	}
	var many []string
	for i := 0; i < 100; i++ {
		many = append(many, fmt.Sprintf("word%d", i))
	}
	if terms, _ = queryTerms(strings.Join(many, " ")); len(terms) > 24 {
		t.Errorf("%d terms", len(terms))
	}
}
