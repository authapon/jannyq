package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// capture replies like Ollama and records the bodies of the requests.
type capture struct {
	mu     sync.Mutex
	bodies []map[string]any
	refuse string // if set, a request carrying a think field is refused with this message
}

func (c *capture) handler(path, reply string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		c.mu.Lock()
		c.bodies = append(c.bodies, m)
		c.mu.Unlock()
		if _, has := m["think"]; has && c.refuse != "" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"`+c.refuse+`"}`)
			return
		}
		io.WriteString(w, reply)
	}
}

const ollamaReply = `{"message":{"role":"assistant","content":"ok"},"done":true}`
const oaiReply = `{"choices":[{"message":{"content":"ok"}}]}`

var askOnce = Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}

func TestOllamaThinkOption(t *testing.T) {
	for name, tc := range map[string]struct {
		think   *bool
		want    any // value of "think" in the request, nil = absent
		present bool
	}{
		"unset": {nil, nil, false},
		"off":   {ptr(false), false, true},
		"on":    {ptr(true), true, true},
	} {
		c := &capture{}
		srv := httptest.NewServer(c.handler("/api/chat", ollamaReply))
		o := &Ollama{BaseURL: srv.URL, Client: srv.Client(), Think: tc.think}
		if _, err := o.Chat(context.Background(), askOnce); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		srv.Close()
		got, has := c.bodies[0]["think"]
		if has != tc.present || got != tc.want {
			t.Errorf("%s: think = %v (present %v), want %v (present %v)", name, got, has, tc.want, tc.present)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestOllamaModelThatCannotThinkIsAskedWithoutTheOption(t *testing.T) {
	c := &capture{refuse: `\"m\" does not support thinking`}
	srv := httptest.NewServer(c.handler("/api/chat", ollamaReply))
	defer srv.Close()
	o := &Ollama{BaseURL: srv.URL, Client: srv.Client(), Think: ptr(true)}
	if _, err := o.Chat(context.Background(), askOnce); err != nil {
		t.Fatalf("the request must be repeated without think: %v", err)
	}
	if len(c.bodies) != 2 {
		t.Fatalf("%d requests, want the refused one and the repeat", len(c.bodies))
	}
	if _, has := c.bodies[1]["think"]; has {
		t.Error("the repeat still carries think")
	}
	// later requests do not try again
	if _, err := o.Chat(context.Background(), askOnce); err != nil {
		t.Fatal(err)
	}
	if len(c.bodies) != 3 {
		t.Errorf("%d requests: the refusal must be remembered", len(c.bodies))
	}
	if _, has := c.bodies[2]["think"]; has {
		t.Error("think is sent again after the model refused it")
	}

	// think=false is never refused for that reason, and an unrelated 400 is an error
	c2 := &capture{refuse: "something else is wrong"}
	srv2 := httptest.NewServer(c2.handler("/api/chat", ollamaReply))
	defer srv2.Close()
	o2 := &Ollama{BaseURL: srv2.URL, Client: srv2.Client(), Think: ptr(false)}
	if _, err := o2.Chat(context.Background(), askOnce); err == nil || !strings.Contains(err.Error(), "something else") {
		t.Errorf("err = %v", err)
	}
	if len(c2.bodies) != 1 {
		t.Errorf("an unrelated refusal was retried (%d requests)", len(c2.bodies))
	}
}

func TestExtraBodyIsAddedToChatRequests(t *testing.T) {
	// Ollama: top-level fields are added, "options" are merged with the ones the bot sets
	c := &capture{}
	srv := httptest.NewServer(c.handler("/api/chat", ollamaReply))
	defer srv.Close()
	o := &Ollama{BaseURL: srv.URL, Client: srv.Client(), ExtraBody: map[string]any{
		"keep_alive": "30m", "options": map[string]any{"num_predict": 256, "num_ctx": 999},
	}}
	req := askOnce
	req.ContextSize = 4096
	if _, err := o.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	b := c.bodies[0]
	opts, _ := b["options"].(map[string]any)
	if b["keep_alive"] != "30m" || opts["num_predict"] != 256.0 || opts["num_ctx"] != 999.0 {
		t.Errorf("body = %v", b) // the operator's num_ctx wins over the bot's
	}
	if b["model"] != "m" || b["stream"] != false {
		t.Errorf("the request itself was lost: %v", b)
	}

	// OpenAI-compatible: reasoning_effort and a template switch for vLLM or llama.cpp
	c2 := &capture{}
	srv2 := httptest.NewServer(c2.handler("/chat/completions", oaiReply))
	defer srv2.Close()
	oa := &OpenAI{BaseURL: srv2.URL, Client: srv2.Client(), ReasoningEffort: "none",
		ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}}
	if _, err := oa.Chat(context.Background(), askOnce); err != nil {
		t.Fatal(err)
	}
	b = c2.bodies[0]
	kw, _ := b["chat_template_kwargs"].(map[string]any)
	if b["reasoning_effort"] != "none" || kw["enable_thinking"] != false || b["model"] != "m" {
		t.Errorf("body = %v", b)
	}

	// nothing configured: nothing added
	c3 := &capture{}
	srv3 := httptest.NewServer(c3.handler("/chat/completions", oaiReply))
	defer srv3.Close()
	if _, err := (&OpenAI{BaseURL: srv3.URL, Client: srv3.Client()}).Chat(context.Background(), askOnce); err != nil {
		t.Fatal(err)
	}
	if _, has := c3.bodies[0]["reasoning_effort"]; has {
		t.Error("reasoning_effort is sent without being asked for")
	}
}
