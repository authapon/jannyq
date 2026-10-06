package channel

import (
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSplit(t *testing.T) {
	if got := Split("  ", 10); got != nil {
		t.Errorf("blank → %v", got)
	}
	if got := Split("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("short → %v", got)
	}

	para := strings.Repeat("a", 60) + "\n\n" + strings.Repeat("b", 60)
	got := Split(para, 100)
	if len(got) != 2 || got[0] != strings.Repeat("a", 60) || got[1] != strings.Repeat("b", 60) {
		t.Errorf("paragraph split: %q", got)
	}

	words := strings.Repeat("word ", 50)
	for _, p := range Split(words, 40) {
		if utf8.RuneCountInString(p) > 40 || strings.HasPrefix(p, " ") || strings.HasSuffix(p, " ") {
			t.Errorf("bad part %q", p)
		}
	}

	// Thai has no spaces: hard cut on rune boundaries, nothing lost
	thai := strings.Repeat("สวัสดี", 100)
	parts := Split(thai, 50)
	var joined strings.Builder
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 50 || !utf8.ValidString(p) {
			t.Fatalf("bad thai part (%d runes)", utf8.RuneCountInString(p))
		}
		joined.WriteString(p)
	}
	if joined.String() != thai {
		t.Error("content lost while splitting")
	}
}

func TestOrdererKeepsArrivalOrderPerChat(t *testing.T) {
	o := NewOrderer()
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wait, accepted := o.Enter("chat") // in arrival order, before the goroutine starts
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer accepted()
			if wait != nil {
				<-wait
			}
			time.Sleep(time.Duration((i*7)%5) * time.Millisecond) // uneven work must not change the order
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			accepted()
		}()
	}
	wg.Wait()
	for i, got := range order {
		if got != i {
			t.Fatalf("order = %v", order)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.tail) != 0 {
		t.Errorf("%d chats are still tracked after everything finished", len(o.tail))
	}
}

func TestOrdererChatsDoNotWaitForEachOther(t *testing.T) {
	o := NewOrderer()
	_, slowAccepted := o.Enter("slow")
	wait, accepted := o.Enter("fast")
	if wait != nil {
		t.Error("the first message of a chat has nothing to wait for")
	}
	accepted()
	waitSlow, accepted2 := o.Enter("slow")
	select {
	case <-waitSlow:
		t.Fatal("must wait for the first slow message")
	case <-time.After(30 * time.Millisecond):
	}
	slowAccepted()
	select {
	case <-waitSlow:
	case <-time.After(time.Second):
		t.Fatal("not released after the first message was accepted")
	}
	accepted2()
}

func TestOrdererAcceptedIsIdempotentAndAFailedMessageDoesNotBlockOthers(t *testing.T) {
	o := NewOrderer()
	_, a1 := o.Enter("c")
	w2, a2 := o.Enter("c")
	w3, a3 := o.Enter("c")
	a1()
	a1()
	a1() // repeated calls are harmless
	<-w2
	// the second message never reaches "accepted" by itself (a crash, a cancelled
	// context): the deferred call of its goroutine still releases the third
	a2()
	select {
	case <-w3:
	case <-time.After(time.Second):
		t.Fatal("a message that finished without being accepted blocked the next one")
	}
	a3()
}
