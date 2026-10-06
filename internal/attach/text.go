package attach

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	xunicode "golang.org/x/text/encoding/unicode"
)

// text reads a plain text file: UTF-8, UTF-16 with a byte order mark, or
// Windows-874 (Thai) when the bytes are not valid UTF-8.
func (p *Processor) text(res *Result, data []byte) error {
	s, enc := decodeText(data)
	if enc != "" {
		res.Note = joinNote(res.Note, "decoded from "+enc)
	}
	s = cleanText(s)
	p.finishText(res, paginate(s, p.cfg.PageChars))
	if nonBlank(res.Text) == 0 {
		return ErrCorrupt
	}
	res.Original = data
	return nil
}

func decodeText(data []byte) (string, string) {
	switch {
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		return string(data[3:]), ""
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}), bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		d := xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM).NewDecoder()
		if out, err := d.Bytes(data); err == nil {
			return string(out), "UTF-16"
		}
	}
	if utf8.Valid(data) {
		return string(data), ""
	}
	if thaiLegacy(data) {
		if out, err := charmap.Windows874.NewDecoder().Bytes(data); err == nil {
			return string(out), "Windows-874"
		}
	}
	return strings.ToValidUTF8(string(data), "�"), "unknown encoding"
}

// cleanText normalises line endings and drops control characters that would
// confuse the model or the page separator.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\f':
			return '\n'
		case unicode.IsControl(r), r == ' ', r == ' ', unicode.Is(unicode.Cf, r) && r != '​' && r != '‍' && r != '‌':
			return -1
		}
		return r
	}, s)
}

// paginate splits text into pages of about size characters, breaking at line
// ends where it can, and joins them with PageSep.
func paginate(s string, size int) string {
	if size <= 0 || utf8.RuneCountInString(s) <= size {
		return s
	}
	var pages []string
	var cur strings.Builder
	curN := 0
	flush := func() {
		if curN > 0 {
			pages = append(pages, cur.String())
			cur.Reset()
			curN = 0
		}
	}
	for _, line := range strings.SplitAfter(s, "\n") {
		n := utf8.RuneCountInString(line)
		if curN+n > size {
			flush()
		}
		for n > size { // one very long line (minified JSON, ...)
			r := []rune(line)
			pages = append(pages, string(r[:size]))
			line = string(r[size:])
			n = len(r) - size
		}
		cur.WriteString(line)
		curN += n
	}
	flush()
	return strings.Join(pages, PageSep)
}
