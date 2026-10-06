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
	ext := strings.ToLower(filepath.Ext(name))
	declaredText := strings.HasPrefix(strings.ToLower(declaredMIME), "text/") ||
		strings.Contains(strings.ToLower(declaredMIME), "json") || strings.Contains(strings.ToLower(declaredMIME), "xml")
	if (textExtensions[ext] || declaredText) && looksLikeText(data) {
		return KindText, "text/plain"
	}
	return "", ""
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

// looksLikeText rejects binary data: text may not contain NUL bytes (except in
// UTF-16, which has a byte order mark) and must be UTF-8 or a recognisable
// legacy encoding.
func looksLikeText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.HasPrefix(head, []byte{0xff, 0xfe}) || bytes.HasPrefix(head, []byte{0xfe, 0xff}) {
		return true // UTF-16 with a byte order mark
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	if utf8.Valid(head) || utf8.Valid(head[:len(head)-min(len(head), 3)]) {
		return true
	}
	return thaiLegacy(head)
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
