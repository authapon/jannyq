package sandbox

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHeadTailBufferSmall(t *testing.T) {
	b := newHeadTailBuffer(100)
	b.Write([]byte("hello "))
	b.Write([]byte("world"))
	out, omitted := b.Render()
	if out != "hello world" || omitted != 0 {
		t.Errorf("out=%q omitted=%d", out, omitted)
	}
}

func TestHeadTailBufferKeepsHeadAndTail(t *testing.T) {
	b := newHeadTailBuffer(100)
	var all strings.Builder
	for i := 0; i < 1000; i++ {
		line := "line " + strings.Repeat("x", i%7) + "\n"
		all.WriteString(line)
		b.Write([]byte(line))
	}
	out, omitted := b.Render()
	full := all.String()
	if omitted <= 0 || !strings.Contains(out, "bytes omitted") {
		t.Fatalf("omitted=%d out=%q", omitted, out)
	}
	if !strings.HasPrefix(out, full[:50]) || !strings.HasSuffix(out, full[len(full)-30:]) {
		t.Errorf("head/tail not preserved: %q", out)
	}
	if len(out) > 100+60 {
		t.Errorf("output too large: %d bytes", len(out))
	}
	if int64(len(full)) < omitted {
		t.Errorf("omitted %d exceeds total %d", omitted, len(full))
	}
}

func TestHeadTailBufferSingleHugeWrite(t *testing.T) {
	b := newHeadTailBuffer(64)
	big := strings.Repeat("a", 5000) + "END"
	n, err := b.Write([]byte(big))
	if n != len(big) || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	out, omitted := b.Render()
	if !strings.HasSuffix(out, "aaEND") || omitted < 4900 {
		t.Errorf("out=%q omitted=%d", out, omitted)
	}
}

func TestHeadTailBufferIsValidUTF8(t *testing.T) {
	b := newHeadTailBuffer(64)
	for i := 0; i < 200; i++ {
		b.Write([]byte("สวัสดี"))
	}
	b.Write([]byte{0xff, 0xfe, 0x00, 'o', 'k'})
	out, _ := b.Render()
	if !utf8.ValidString(out) || strings.ContainsRune(out, 0) {
		t.Errorf("invalid output: %q", out)
	}
	if !strings.HasSuffix(out, "ok") {
		t.Errorf("tail lost: %q", out)
	}
}
