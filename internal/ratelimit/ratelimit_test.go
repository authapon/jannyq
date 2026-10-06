package ratelimit

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(2, time.Minute)
	l.now = func() time.Time { return now }
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") {
		t.Fatal("limit not enforced")
	}
	if !l.Allow("b") {
		t.Error("limit must be per key")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("a") {
		t.Error("window should have slid")
	}
	if !New(0, time.Minute).Allow("x") {
		t.Error("0 means unlimited")
	}
}
