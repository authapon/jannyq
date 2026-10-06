package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
)

// fill appends question/answer turns numbered first..last.
func fill(t *testing.T, s *session.Session, first, last int, pad string) {
	t.Helper()
	for i := first; i <= last; i++ {
		err := s.Append(ctx,
			llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("question %d %s", i, pad)},
			llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("answer %d %s", i, pad)})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func summariser(out string, seen *[]llm.Request) func(llm.Request) (*llm.Response, error) {
	return func(req llm.Request) (*llm.Response, error) {
		*seen = append(*seen, req)
		return say(out)(req)
	}
}

func TestCompactByMessageCount(t *testing.T) {
	var seen []llm.Request
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){summariser("- SUMMARY", &seen)}}
	a := newAgent(p, Config{CompactAfter: 10, CompactKeep: 4})
	withSession(t, func(s *session.Session) {
		fill(t, s, 1, 4, "") // 8 messages: below threshold
		if done, err := a.MaybeCompact(ctx, s); err != nil || done {
			t.Fatalf("should not compact yet: %v %v", done, err)
		}
		fill(t, s, 5, 7, "") // 14 messages: above threshold
		done, err := a.MaybeCompact(ctx, s)
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		h := history(t, s)
		if len(h) != 4 || h[0].Role != llm.RoleUser || h[0].Content != "question 6 " {
			t.Errorf("kept history = %+v", h)
		}
		if sum, _ := s.Summary(ctx); sum != "- SUMMARY" {
			t.Errorf("summary = %q", sum)
		}
	})
	if len(seen) != 1 {
		t.Fatalf("summariser calls = %d", len(seen))
	}
	prompt := seen[0].Messages[1].Content
	for _, want := range []string{"User: question 1", "Assistant: answer 5", "(none yet)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("summariser prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "question 6") {
		t.Error("kept messages must not be summarised")
	}
	if len(seen[0].Tools) != 0 {
		t.Error("summariser must not get tools")
	}
}

func TestCompactFoldsPreviousSummary(t *testing.T) {
	var seen []llm.Request
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		summariser("- first", &seen), summariser("- second", &seen),
	}}
	a := newAgent(p, Config{CompactAfter: 6, CompactKeep: 2})
	withSession(t, func(s *session.Session) {
		fill(t, s, 1, 4, "")
		if _, err := a.MaybeCompact(ctx, s); err != nil {
			t.Fatal(err)
		}
		fill(t, s, 5, 8, "")
		if done, err := a.MaybeCompact(ctx, s); err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		if sum, _ := s.Summary(ctx); sum != "- second" {
			t.Errorf("summary = %q", sum)
		}
	})
	if !strings.Contains(seen[1].Messages[1].Content, "- first") {
		t.Error("second compaction must receive the earlier summary")
	}
}

func TestCompactByTokenPressure(t *testing.T) {
	var seen []llm.Request
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){summariser("- S", &seen)}}
	// few messages, but big: 6 turns of ~400 chars against a 2000-token window
	a := newAgent(p, Config{ContextSize: 2000, CompactAfter: 200, CompactKeep: 20})
	withSession(t, func(s *session.Session) {
		fill(t, s, 1, 6, strings.Repeat("word ", 80))
		done, err := a.MaybeCompact(ctx, s)
		if err != nil || !done {
			t.Fatalf("done=%v err=%v", done, err)
		}
		h := history(t, s)
		if len(h) == 0 || len(h) >= 12 {
			t.Errorf("kept %d messages", len(h))
		}
		if est := estimateStoredMessages(h); float64(est) > keptTokenShare*2000+200 {
			t.Errorf("kept tail is %d tokens, too big for the window", est)
		}
		if h[0].Role != llm.RoleUser {
			t.Errorf("tail must start at a user message, got %s", h[0].Role)
		}
	})
}

func estimateStoredMessages(msgs []llm.Message) int { return llm.EstimateMessages(msgs) }

func TestNoCompactionWhenAllFitsInKeepWindow(t *testing.T) {
	p := &fakeProvider{} // any LLM call would fail the test
	a := newAgent(p, Config{CompactAfter: 4, CompactKeep: 3})
	withSession(t, func(s *session.Session) {
		_ = s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "a"}, llm.Message{Role: llm.RoleAssistant, Content: "b"})
		if done, err := a.Compact(ctx, s, 10); err != nil || done {
			t.Errorf("done=%v err=%v", done, err)
		}
	})
}

func TestCompactNeverSplitsToolExchange(t *testing.T) {
	var seen []llm.Request
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){summariser("- S", &seen)}}
	a := newAgent(p, Config{CompactAfter: 4, CompactKeep: 3})
	withSession(t, func(s *session.Session) {
		_ = s.Append(ctx,
			llm.Message{Role: llm.RoleUser, Content: "q1"},
			llm.Message{Role: llm.RoleAssistant, Content: "a1"},
			llm.Message{Role: llm.RoleUser, Content: "q2"},
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "t", Name: "echo", Arguments: []byte(`{}`)}}},
			llm.Message{Role: llm.RoleTool, ToolCallID: "t", Name: "echo", Content: "res"},
			llm.Message{Role: llm.RoleAssistant, Content: "a2"})
		if _, err := a.Compact(ctx, s, 3); err != nil {
			t.Fatal(err)
		}
		h := history(t, s)
		if h[0].Role != llm.RoleUser || h[0].Content != "q2" || len(h) != 4 {
			t.Errorf("kept = %+v", h)
		}
	})
}

func TestCompactBatchesLongHistory(t *testing.T) {
	var seen []llm.Request
	step := summariser("- S", &seen)
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){step, step, step, step, step, step, step, step}}
	// tiny window → tiny batches
	a := newAgent(p, Config{ContextSize: 2600, CompactAfter: 200, CompactKeep: 2})
	withSession(t, func(s *session.Session) {
		fill(t, s, 1, 8, strings.Repeat("lorem ipsum ", 60))
		if _, err := a.Compact(ctx, s, 2); err != nil {
			t.Fatal(err)
		}
	})
	if len(seen) < 2 {
		t.Errorf("expected several summariser batches, got %d", len(seen))
	}
}

func TestCompactFailureLeavesHistoryIntact(t *testing.T) {
	p := &fakeProvider{} // summariser call fails
	a := newAgent(p, Config{CompactAfter: 4, CompactKeep: 2})
	withSession(t, func(s *session.Session) {
		fill(t, s, 1, 5, "")
		if _, err := a.MaybeCompact(ctx, s); err == nil {
			t.Fatal("expected error")
		}
		if n, _ := s.Count(ctx); n != 10 {
			t.Errorf("history changed after failed compaction: %d", n)
		}
	})
}
