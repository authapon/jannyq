package sandbox

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// headTailBuffer keeps the first and last bytes written and counts the rest.
// It never blocks the writer, so a chatty command cannot stall on a full pipe
// or exhaust memory.
type headTailBuffer struct {
	mu      sync.Mutex
	headMax int
	tailMax int
	head    []byte
	tail    []byte // ring buffer once full
	tailPos int
	tailLen int
	total   int64
}

func newHeadTailBuffer(max int) *headTailBuffer {
	if max < 16 {
		max = 16
	}
	h := max * 6 / 10
	return &headTailBuffer{headMax: h, tailMax: max - h, tail: make([]byte, max-h)}
}

func (b *headTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	if len(b.head) < b.headMax {
		take := b.headMax - len(b.head)
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
		p = p[take:]
	}
	if len(p) > b.tailMax { // only the last tailMax bytes can survive
		p = p[len(p)-b.tailMax:]
	}
	for _, c := range p {
		b.tail[b.tailPos] = c
		b.tailPos = (b.tailPos + 1) % b.tailMax
		if b.tailLen < b.tailMax {
			b.tailLen++
		}
	}
	return n, nil
}

// Render returns the captured output, with a marker where bytes were
// dropped, and the number of omitted bytes.
func (b *headTailBuffer) Render() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail := make([]byte, 0, b.tailLen)
	if b.tailLen < b.tailMax {
		tail = append(tail, b.tail[:b.tailLen]...)
	} else {
		tail = append(tail, b.tail[b.tailPos:]...)
		tail = append(tail, b.tail[:b.tailPos]...)
	}
	head := b.head
	omitted := b.total - int64(len(head)) - int64(len(tail))
	if omitted <= 0 {
		return clean(string(head) + string(tail)), 0
	}
	head = trimPartialRuneEnd(head)
	tail = trimPartialRuneStart(tail)
	omitted = b.total - int64(len(head)) - int64(len(tail))
	return clean(string(head)) + fmt.Sprintf("\n[... %d bytes omitted ...]\n", omitted) + clean(string(tail)), omitted
}

func trimPartialRuneEnd(p []byte) []byte {
	for i := 1; i <= utf8.UTFMax && i <= len(p); i++ {
		if utf8.RuneStart(p[len(p)-i]) {
			if !utf8.FullRune(p[len(p)-i:]) {
				return p[:len(p)-i]
			}
			break
		}
	}
	return p
}

func trimPartialRuneStart(p []byte) []byte {
	for i := 0; i < utf8.UTFMax && i < len(p); i++ {
		if utf8.RuneStart(p[i]) {
			return p[i:]
		}
	}
	return p
}

// clean makes output safe to hand to a model: valid UTF-8, no NULs.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "")
}
