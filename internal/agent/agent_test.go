package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/metrics"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/skill"
	"github.com/authapon/jannyq/internal/tool"
)

var ctx = context.Background()

// fakeProvider replays scripted responses and records the requests.
type fakeProvider struct {
	mu       sync.Mutex
	requests []llm.Request
	script   []func(llm.Request) (*llm.Response, error)
}

func (f *fakeProvider) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.script) == 0 {
		return nil, errors.New("fakeProvider: script exhausted")
	}
	next := f.script[0]
	f.script = f.script[1:]
	return next(req)
}

func say(text string) func(llm.Request) (*llm.Response, error) {
	return func(llm.Request) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: text}}, nil
	}
}

func callTool(id, name, args string) func(llm.Request) (*llm.Response, error) {
	return func(llm.Request) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: id, Name: name, Arguments: json.RawMessage(args)},
		}}}, nil
	}
}

type echoTool struct {
	calls []string
	err   error
	out   string
}

func (e *echoTool) Name() string        { return "echo" }
func (e *echoTool) Description() string { return "echo" }
func (e *echoTool) Parameters() []byte  { return []byte(`{"type":"object"}`) }
func (e *echoTool) Execute(_ context.Context, _ tool.CallContext, args []byte) (string, error) {
	e.calls = append(e.calls, string(args))
	if e.err != nil {
		return "", e.err
	}
	if e.out != "" {
		return e.out, nil
	}
	return "echoed " + string(args), nil
}

func newAgent(p llm.Provider, cfg Config, tools ...tool.Tool) *Agent {
	reg := tool.NewRegistry()
	for _, t := range tools {
		reg.Register(t)
	}
	if cfg.Model == "" {
		cfg.Model = "test-model"
	}
	return New(cfg, p, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func withSession(t *testing.T, fn func(*session.Session)) {
	t.Helper()
	m := session.NewManager(t.TempDir(), 4, 4)
	defer m.Close()
	if err := m.With(ctx, "test", "chat", func(s *session.Session) error { fn(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func history(t *testing.T, s *session.Session) []llm.Message {
	t.Helper()
	st, err := s.Messages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]llm.Message, len(st))
	for i := range st {
		out[i] = st[i].Message
	}
	return out
}

func TestSimpleReply(t *testing.T) {
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("  <think>hmm</think>\nHello there  ")}}
	a := newAgent(p, Config{Lang: "th", ContextSize: 4096})
	withSession(t, func(s *session.Session) {
		got, err := a.Reply(ctx, s, Input{Text: "hi", Sender: "Ann"})
		if err != nil || got != "Hello there" {
			t.Fatalf("got %q err %v", got, err)
		}
		h := history(t, s)
		if len(h) != 2 || h[0].Content != "hi" || h[1].Content != "Hello there" {
			t.Errorf("history = %+v", h)
		}
	})
	req := p.requests[0]
	if req.Model != "test-model" || req.ContextSize != 4096 {
		t.Errorf("request = %+v", req)
	}
	sys := req.Messages[0]
	if sys.Role != llm.RoleSystem || !strings.Contains(sys.Content, "Thai") {
		t.Errorf("system prompt = %q", sys.Content)
	}
	if len(req.Tools) != 0 {
		t.Errorf("no tools registered but request has %d", len(req.Tools))
	}
}

func TestLanguageModes(t *testing.T) {
	for mode, want := range map[string]string{
		LangModeDefault:    "reply in Thai unless the user explicitly asks",
		LangModeFollowUser: "reply in the language the user writes in; if unclear, reply in Thai",
	} {
		a := newAgent(&fakeProvider{}, Config{Lang: "th", LangMode: mode})
		if got := a.systemPrompt(Input{}, false); !strings.Contains(got, want) {
			t.Errorf("%s: prompt lacks %q:\n%s", mode, want, got)
		}
	}
}

func TestGroupMessagesCarryWhoAndWhen(t *testing.T) {
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("ok")}}
	a := newAgent(p, Config{Location: time.FixedZone("BST", 3600)})
	withSession(t, func(s *session.Session) {
		sent := time.Date(1997, 7, 16, 18, 20, 44, 0, time.UTC)
		if _, err := a.Reply(ctx, s, Input{Text: "hello", Sender: "Bob", UserID: "77", IsGroup: true, SentAt: sent}); err != nil {
			t.Fatal(err)
		}
		// the stored text is what the user typed; the header is added when the prompt is built
		if h := history(t, s); h[0].Content != "hello" {
			t.Errorf("stored user message = %q", h[0].Content)
		}
		if !s.IsGroup(ctx) {
			t.Error("the chat was not marked as a group")
		}
	})
	req := p.requests[0]
	want := "[1997-07-16T19:20:44+01:00] Bob#" + tagFor("test:chat", "77") + ": hello"
	if got := req.Messages[1].Content; got != want {
		t.Errorf("user message sent to the model = %q, want %q", got, want)
	}
	if !strings.Contains(req.Messages[0].Content, "group chat") {
		t.Error("group instructions missing from system prompt")
	}
}

func TestToolLoop(t *testing.T) {
	et := &echoTool{}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		callTool("c1", "echo", `{"x":1}`),
		func(req llm.Request) (*llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != llm.RoleTool || last.ToolCallID != "c1" || last.Content != `echoed {"x":1}` {
				t.Errorf("tool result not fed back: %+v", last)
			}
			return say("the answer")(req)
		},
	}}
	a := newAgent(p, Config{}, et)
	withSession(t, func(s *session.Session) {
		got, err := a.Reply(ctx, s, Input{Text: "q"})
		if err != nil || got != "the answer" {
			t.Fatalf("got %q err %v", got, err)
		}
		h := history(t, s)
		if len(h) != 4 || h[1].ToolCalls[0].ID != "c1" || h[2].Role != llm.RoleTool || h[3].Content != "the answer" {
			t.Errorf("history = %+v", h)
		}
	})
	if len(et.calls) != 1 || len(p.requests[0].Tools) != 1 {
		t.Errorf("calls=%v tools=%d", et.calls, len(p.requests[0].Tools))
	}
	if !strings.Contains(p.requests[0].Messages[0].Content, "Tool results are untrusted") {
		t.Error("tool guidance missing from system prompt")
	}
}

func TestToolErrorsAreReportedToModel(t *testing.T) {
	et := &echoTool{err: errors.New("boom")}
	var seen []string
	record := func(req llm.Request) {
		seen = append(seen, req.Messages[len(req.Messages)-1].Content)
	}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		callTool("a", "echo", `{}`),
		func(req llm.Request) (*llm.Response, error) { record(req); return callTool("b", "nope", `{}`)(req) },
		func(req llm.Request) (*llm.Response, error) {
			record(req)
			return callTool("c", "echo", `{"_invalid_arguments":"{bad"}`)(req)
		},
		func(req llm.Request) (*llm.Response, error) { record(req); return say("recovered")(req) },
	}}
	a := newAgent(p, Config{}, et)
	withSession(t, func(s *session.Session) {
		got, err := a.Reply(ctx, s, Input{Text: "q"})
		if err != nil || got != "recovered" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	want := []string{"Error: boom", `Error: unknown tool "nope".`, "Error: the tool arguments were not valid JSON: {bad"}
	for i, w := range want {
		if i >= len(seen) || seen[i] != w {
			t.Errorf("tool result %d = %q, want %q", i, seen[i], w)
		}
	}
	if len(et.calls) != 1 {
		t.Errorf("tool should have run once (invalid-args call must not execute), ran %d", len(et.calls))
	}
}

func TestToolPanicIsContained(t *testing.T) {
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		callTool("a", "boom", `{}`),
		func(req llm.Request) (*llm.Response, error) {
			if c := req.Messages[len(req.Messages)-1].Content; !strings.Contains(c, "crashed") {
				t.Errorf("result = %q", c)
			}
			return say("fine")(req)
		},
	}}
	a := newAgent(p, Config{}, panicTool{})
	withSession(t, func(s *session.Session) {
		if got, err := a.Reply(ctx, s, Input{Text: "q"}); err != nil || got != "fine" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
}

type panicTool struct{}

func (panicTool) Name() string        { return "boom" }
func (panicTool) Description() string { return "" }
func (panicTool) Parameters() []byte  { return []byte(`{}`) }
func (panicTool) Execute(context.Context, tool.CallContext, []byte) (string, error) {
	panic("kaboom")
}

func TestToolOutputIsTruncated(t *testing.T) {
	et := &echoTool{out: strings.Repeat("x", 500)}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		callTool("a", "echo", `{}`),
		func(req llm.Request) (*llm.Response, error) {
			c := req.Messages[len(req.Messages)-1].Content
			if !strings.Contains(c, "[truncated: 400 more characters]") {
				t.Errorf("not truncated: %d chars", len(c))
			}
			return say("ok")(req)
		},
	}}
	a := newAgent(p, Config{ToolMaxOutput: 100}, et)
	withSession(t, func(s *session.Session) { _, _ = a.Reply(ctx, s, Input{Text: "q"}) })
}

func TestMaxStepsForcesFinalAnswer(t *testing.T) {
	et := &echoTool{}
	loop := func(req llm.Request) (*llm.Response, error) {
		return callTool("x"+fmt.Sprint(len(req.Messages)), "echo", `{}`)(req)
	}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		loop, loop,
		func(req llm.Request) (*llm.Response, error) {
			if len(req.Tools) != 0 || !strings.Contains(req.Messages[0].Content, "used all available tool calls") {
				t.Errorf("final step should have no tools and a nudge: tools=%d", len(req.Tools))
			}
			return say("best effort")(req)
		},
	}}
	a := newAgent(p, Config{MaxSteps: 2}, et)
	withSession(t, func(s *session.Session) {
		got, err := a.Reply(ctx, s, Input{Text: "q"})
		if err != nil || got != "best effort" {
			t.Fatalf("got %q err %v", got, err)
		}
	})
	if len(et.calls) != 2 {
		t.Errorf("tool calls = %d, want 2", len(et.calls))
	}
}

func TestExtraToolCallsAreSkipped(t *testing.T) {
	et := &echoTool{}
	many := func(llm.Request) (*llm.Response, error) {
		var tcs []llm.ToolCall
		for i := 0; i < maxToolCallsPerStep+2; i++ {
			tcs = append(tcs, llm.ToolCall{ID: fmt.Sprint("c", i), Name: "echo", Arguments: json.RawMessage(`{}`)})
		}
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: tcs}}, nil
	}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){many, say("done")}}
	a := newAgent(p, Config{}, et)
	withSession(t, func(s *session.Session) {
		if _, err := a.Reply(ctx, s, Input{Text: "q"}); err != nil {
			t.Fatal(err)
		}
		// every call still gets a result, so the history stays valid
		h := history(t, s)
		if len(h) != 1+1+maxToolCallsPerStep+2+1 {
			t.Errorf("history len = %d", len(h))
		}
	})
	if len(et.calls) != maxToolCallsPerStep {
		t.Errorf("ran %d calls, want %d", len(et.calls), maxToolCallsPerStep)
	}
}

func TestEmptyResponse(t *testing.T) {
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("   <think>only thinking</think> ")}}
	a := newAgent(p, Config{})
	withSession(t, func(s *session.Session) {
		if _, err := a.Reply(ctx, s, Input{Text: "q"}); !errors.Is(err, ErrEmptyResponse) {
			t.Errorf("err = %v", err)
		}
	})
}

func TestProviderErrorKeepsUserMessage(t *testing.T) {
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		func(llm.Request) (*llm.Response, error) { return nil, errors.New("down") },
	}}
	a := newAgent(p, Config{})
	withSession(t, func(s *session.Session) {
		if _, err := a.Reply(ctx, s, Input{Text: "q"}); err == nil {
			t.Fatal("expected error")
		}
		if h := history(t, s); len(h) != 1 || h[0].Content != "q" {
			t.Errorf("history = %+v", h)
		}
	})
}

func TestToolsUnsupportedFallsBack(t *testing.T) {
	var withTools []int
	rec := func(req llm.Request) { withTools = append(withTools, len(req.Tools)) }
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		func(req llm.Request) (*llm.Response, error) { rec(req); return nil, llm.ErrToolsUnsupported },
		func(req llm.Request) (*llm.Response, error) { rec(req); return say("no tools needed")(req) },
		func(req llm.Request) (*llm.Response, error) { rec(req); return say("again")(req) },
	}}
	a := newAgent(p, Config{}, &echoTool{})
	withSession(t, func(s *session.Session) {
		if got, err := a.Reply(ctx, s, Input{Text: "q"}); err != nil || got != "no tools needed" {
			t.Fatalf("got %q err %v", got, err)
		}
		if _, err := a.Reply(ctx, s, Input{Text: "q2"}); err != nil {
			t.Fatal(err)
		}
	})
	if fmt.Sprint(withTools) != "[1 0 0]" {
		t.Errorf("tool counts per call = %v, want [1 0 0]", withTools)
	}
}

func TestSummaryIsInjectedIntoSystemPrompt(t *testing.T) {
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("ok")}}
	a := newAgent(p, Config{})
	withSession(t, func(s *session.Session) {
		_ = s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "old"}, llm.Message{Role: llm.RoleAssistant, Content: "old reply"})
		old, _ := s.Messages(ctx)
		_ = s.Compact(ctx, old[1].ID, "- user likes tea")
		if _, err := a.Reply(ctx, s, Input{Text: "new"}); err != nil {
			t.Fatal(err)
		}
	})
	if sys := p.requests[0].Messages[0].Content; !strings.Contains(sys, "- user likes tea") {
		t.Errorf("summary missing:\n%s", sys)
	}
}

type hintTool struct{ echoTool }

func (hintTool) Name() string { return "hinted" }
func (hintTool) Hint() string { return "hinted: use me wisely" }

type fakeSkills []skill.Summary

func (f fakeSkills) Summaries() []skill.Summary { return f }

type loadSkillStub struct{ echoTool }

func (loadSkillStub) Name() string { return "load_skill" }

func TestPromptMentionsOnlyAvailableTools(t *testing.T) {
	a := newAgent(&fakeProvider{}, Config{}, &echoTool{})
	got := a.systemPrompt(Input{}, true)
	for _, bad := range []string{"web_search", "web_fetch", "source URLs"} {
		if strings.Contains(got, bad) {
			t.Errorf("prompt mentions unavailable %q:\n%s", bad, got)
		}
	}
	a = newAgent(&fakeProvider{}, Config{}, webStub("web_search"), webStub("web_fetch"))
	got = a.systemPrompt(Input{}, true)
	for _, want := range []string{"web_search", "web_fetch", "source URLs"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	if a.systemPrompt(Input{}, false) == got {
		t.Error("prompt without tools must not describe them")
	}
}

type webStub string

func (w webStub) Name() string                                                    { return string(w) }
func (webStub) Description() string                                               { return "" }
func (webStub) Parameters() []byte                                                { return []byte(`{}`) }
func (webStub) Execute(context.Context, tool.CallContext, []byte) (string, error) { return "", nil }

func TestPromptIncludesToolHintsAndSkills(t *testing.T) {
	skills := fakeSkills{{Name: "csv", Description: "summarise csv files"}, {Name: "pdf", Description: "read pdfs"}}
	a := newAgent(&fakeProvider{}, Config{Skills: skills}, &hintTool{}, &loadSkillStub{})
	got := a.systemPrompt(Input{}, true)
	for _, want := range []string{"hinted: use me wisely", "- csv: summarise csv files", "- pdf: read pdfs", "call load_skill"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt lacks %q:\n%s", want, got)
		}
	}
	// skills are useless without the load_skill tool, or with no skills
	if got := newAgent(&fakeProvider{}, Config{Skills: skills}, &hintTool{}).systemPrompt(Input{}, true); strings.Contains(got, "csv") {
		t.Error("skills listed although load_skill is not registered")
	}
	if got := newAgent(&fakeProvider{}, Config{Skills: fakeSkills{}}, &loadSkillStub{}).systemPrompt(Input{}, true); strings.Contains(got, "Skills:") {
		t.Error("empty skills section printed")
	}
}

func TestAgentMetrics(t *testing.T) {
	reg := metrics.New()
	inst := metrics.NewInstruments(reg)
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		callTool("1", "echo", `{}`),
		callTool("2", "made_up_tool_name", `{}`),
		func(llm.Request) (*llm.Response, error) {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 7}}, nil
		},
	}}
	a := newAgent(p, Config{Metrics: inst}, &echoTool{})
	withSession(t, func(s *session.Session) {
		if _, err := a.Reply(ctx, s, Input{Text: "go"}); err != nil {
			t.Fatal(err)
		}
	})
	out := reg.Render()
	for _, want := range []string{
		`jannyq_tool_calls_total{tool="echo",result="ok"} 1`,
		`jannyq_tool_calls_total{tool="unknown",result="error"} 1`,
		`jannyq_llm_requests_total{result="ok"} 3`,
		`jannyq_llm_tokens_total{type="prompt"} 100`,
		`jannyq_llm_tokens_total{type="completion"} 7`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "made_up_tool_name") {
		t.Error("a tool name invented by the model became a label")
	}
	// a failing model is counted too
	reg2 := metrics.New()
	b := newAgent(&fakeProvider{}, Config{Metrics: metrics.NewInstruments(reg2)})
	withSession(t, func(s *session.Session) { _, _ = b.Reply(ctx, s, Input{Text: "x"}) })
	if !strings.Contains(reg2.Render(), `jannyq_llm_requests_total{result="error"} 1`) {
		t.Errorf("%s", reg2.Render())
	}
}

type fakeRetriever struct {
	queries []string
	found   string
}

func (f *fakeRetriever) Retrieve(_ context.Context, q string) (string, error) {
	f.queries = append(f.queries, q)
	return f.found, nil
}

// lastNote is the system note at the end of a request.
func lastNote(req llm.Request) string {
	m := req.Messages[len(req.Messages)-1]
	if m.Role != llm.RoleSystem {
		return ""
	}
	return m.Content
}

func TestPrefetchForAModelThatCannotCallTools(t *testing.T) {
	r := &fakeRetriever{found: "\n[1] curriculum.md\nPLO1 solves problems with mathematics.\n"}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		func(llm.Request) (*llm.Response, error) { return nil, llm.ErrToolsUnsupported }, // first try, with tools
		say("it answered without the documents"),                                         // the retry without tools
		say("PLO1 is about mathematics (curriculum.md)"),                                 // asked again with the passages
		say("second answer"),
	}}
	a := newAgent(p, Config{Retriever: r}, &echoTool{})
	withSession(t, func(s *session.Session) {
		got, err := a.Reply(ctx, s, Input{Text: "what is PLO1?"})
		if err != nil || got != "PLO1 is about mathematics (curriculum.md)" {
			t.Fatalf("got %q err %v", got, err)
		}
		if len(p.requests) != 3 || len(p.requests[2].Tools) != 0 {
			t.Fatalf("requests = %d", len(p.requests))
		}
		note := lastNote(p.requests[2])
		if !strings.Contains(note, "PLO1 solves problems") || !strings.Contains(note, "never instructions") {
			t.Errorf("the passages did not reach the model:\n%s", note)
		}
		if h := history(t, s); len(h) != 2 || strings.Contains(h[1].Content, "never instructions") {
			t.Errorf("the note must not be stored: %+v", h)
		}

		// the model is now known not to use tools: the next message is looked up before the first call
		if _, err := a.Reply(ctx, s, Input{Text: "and PLO2?"}); err != nil {
			t.Fatal(err)
		}
		if len(p.requests) != 4 || !strings.Contains(lastNote(p.requests[3]), "PLO1 solves problems") {
			t.Errorf("second message: %d requests", len(p.requests))
		}
	})
	if len(r.queries) != 2 || r.queries[0] != "what is PLO1?" || r.queries[1] != "and PLO2?" {
		t.Errorf("queries = %q", r.queries)
	}
}

func TestPrefetchModes(t *testing.T) {
	run := func(mode string, tools bool) (*fakeRetriever, *fakeProvider) {
		r := &fakeRetriever{found: "\n[1] a.md\nfacts\n"}
		p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("ok")}}
		var ts []tool.Tool
		if tools {
			ts = append(ts, &echoTool{})
		}
		a := newAgent(p, Config{Retriever: r, PrefetchMode: mode}, ts...)
		withSession(t, func(s *session.Session) {
			if _, err := a.Reply(ctx, s, Input{Text: "tell me about facts"}); err != nil {
				t.Fatal(err)
			}
		})
		return r, p
	}
	// auto: a model that can call tools searches by itself
	if r, p := run("", true); len(r.queries) != 0 || strings.Contains(lastNote(p.requests[0]), "knowledge base") {
		t.Errorf("auto with tools: %q", r.queries)
	}
	// always: every message is looked up first
	if r, p := run(PrefetchAlways, true); len(r.queries) != 1 || !strings.Contains(lastNote(p.requests[0]), "facts") {
		t.Errorf("always: %q", r.queries)
	}
	// off: never
	if r, _ := run(PrefetchOff, true); len(r.queries) != 0 {
		t.Errorf("off: %q", r.queries)
	}
	// nothing found: no note
	r := &fakeRetriever{}
	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("ok")}}
	a := newAgent(p, Config{Retriever: r, PrefetchMode: PrefetchAlways})
	withSession(t, func(s *session.Session) { _, _ = a.Reply(ctx, s, Input{Text: "tell me about facts"}) })
	if lastNote(p.requests[0]) != "" {
		t.Error("an empty search must add nothing")
	}
	// too short a message is not searched
	r = &fakeRetriever{found: "x"}
	p = &fakeProvider{script: []func(llm.Request) (*llm.Response, error){say("ok")}}
	a = newAgent(p, Config{Retriever: r, PrefetchMode: PrefetchAlways})
	withSession(t, func(s *session.Session) { _, _ = a.Reply(ctx, s, Input{Text: "ok"}) })
	if len(r.queries) != 0 {
		t.Errorf("queries = %q", r.queries)
	}
}

func TestContextUse(t *testing.T) {
	if got := (ContextUse{Used: 2400, Size: 20000}).String(); got != "2400/20000" {
		t.Errorf("%q", got)
	}
	if got := (ContextUse{Used: 2400, Size: 20000, Estimated: true}).String(); got != "~2400/20000" {
		t.Errorf("%q", got)
	}
	if got := (ContextUse{Used: 2400}).String(); got != "2400/?" {
		t.Errorf("%q", got)
	}

	p := &fakeProvider{script: []func(llm.Request) (*llm.Response, error){
		callTool("c1", "echo", `{}`), // the first call reports its tokens...
		func(llm.Request) (*llm.Response, error) { // ...the last one grows with the tool result
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "done"}, Usage: llm.Usage{PromptTokens: 900, CompletionTokens: 100}}, nil
		},
	}}
	a := newAgent(p, Config{ContextSize: 8192}, &echoTool{})
	withSession(t, func(s *session.Session) {
		if _, ok := a.ContextUse(s.Channel + ":" + s.ChatID); ok {
			t.Error("nothing has been used yet")
		}
		if _, err := a.Reply(ctx, s, Input{Text: "q"}); err != nil {
			t.Fatal(err)
		}
		u, ok := a.ContextUse(s.Channel + ":" + s.ChatID)
		if !ok || u.Used != 1000 || u.Size != 8192 || u.Estimated {
			t.Errorf("use = %+v (the latest call counts, not the first), ok=%v", u, ok)
		}
		if _, ok := a.ContextUse("other:chat"); ok {
			t.Error("another chat has no use")
		}
	})
}
