package attach

import (
	"bytes"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Kinds of attachment the bot understands.
const (
	KindImage = "image"
	KindPDF   = "pdf"
	KindText  = "text"
)

// textExtensions are file names read as text even when the sender's app
// declares nothing useful (Telegram often says application/octet-stream).
var textExtensions = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".tsv": true, ".json": true, ".jsonl": true,
	".xml": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".log": true, ".html": true,
	".htm": true, ".css": true, ".js": true, ".ts": true, ".py": true, ".go": true, ".java": true, ".c": true,
	".h": true, ".cpp": true, ".rs": true, ".sh": true, ".sql": true, ".srt": true, ".vtt": true, ".tex": true,
	".rtf": false, // not plain text
}

// sniff decides what a file is from its content. The declared type and name
// are not trusted for images and PDFs; they only help to recognise text, which
// has no signature.
func sniff(name, declaredMIME string, data []byte) (kind, mime string) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return KindImage, "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return KindImage, "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return KindImage, "image/gif"
	case len(data) > 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return KindImage, "image/webp"
	case hasPDFHeader(data):
		return KindPDF, "application/pdf"
	}
	if claimsText(name, declaredMIME) && textEncoding(data) != "" {
		return KindText, "text/plain"
	}
	return "", ""
}

// claimsText reports whether the name or the declared type say "text". Text has
// no signature, so they are all there is to go on before looking at the bytes.
func claimsText(name, declaredMIME string) bool {
	mime := strings.ToLower(declaredMIME)
	return textExtensions[strings.ToLower(filepath.Ext(name))] ||
		strings.HasPrefix(mime, "text/") || strings.Contains(mime, "json") || strings.Contains(mime, "xml")
}

// hasPDFHeader reports whether "%PDF-" appears in the first kilobyte, where
// the specification allows junk before it.
func hasPDFHeader(data []byte) bool {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(head, []byte("%PDF-"))
}

// sampleSize is how much of a file is looked at to tell text from binary data.
const sampleSize = 8192

// textEncoding says how a file is encoded when it is text ("utf-8", "utf-16" or
// "windows-874"), and "" when it is not: it may not contain NUL bytes (except
// UTF-16, which has a byte order mark) and must be UTF-8, with at most a few
// damaged bytes, or a recognisable Thai legacy encoding.
func textEncoding(data []byte) string {
	if len(data) == 0 {
		return "utf-8"
	}
	switch {
	case bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}):
		return "utf-8"
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}), bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		return "utf-16"
	}
	head := data
	if len(head) > sampleSize {
		// the sample ends wherever it ends, often in the middle of a character
		head = trimPartialRune(head[:sampleSize])
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return ""
	}
	if mostlyUTF8(head) { // before the legacy guess: UTF-8 Thai also has bytes in the Thai range of Windows-874
		return "utf-8"
	}
	if thaiLegacy(head) {
		return "windows-874"
	}
	return ""
}

// trimPartialRune drops an incomplete character from the end of b.
func trimPartialRune(b []byte) []byte {
	for i := 1; i <= 3 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c&0xC0 == 0x80 {
			continue // a continuation byte: look further back for its first byte
		}
		if c >= 0xC0 && !utf8.FullRune(b[len(b)-i:]) {
			return b[:len(b)-i]
		}
		return b
	}
	return b
}

// mostlyUTF8 accepts text in which no more than one character in a hundred is
// damaged, as happens to files that were pasted together or edited by tools that
// disagree about encodings.
func mostlyUTF8(b []byte) bool {
	bad, total := 0, 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			bad++
		}
		total++
		b = b[size:]
	}
	return bad*100 <= total
}

// thaiLegacy guesses Windows-874 / TIS-620, still common in Thai spreadsheets:
// most high bytes fall in the Thai letter range.
func thaiLegacy(data []byte) bool {
	high, thai := 0, 0
	for _, b := range data {
		if b >= 0x80 {
			high++
			if b >= 0xA1 && b <= 0xFB {
				thai++
			}
		}
	}
	return high > 0 && thai*10 >= high*9
}
