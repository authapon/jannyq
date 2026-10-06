package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func init() { retryBase = time.Millisecond }

func weatherTool() ToolDef {
	return ToolDef{Name: "weather", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func TestOpenAIChatToolCall(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[
			{"id":"c1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Bangkok\"}"}},
			{"type":"function","function":{"name":"weather","arguments":"not json"}}]}}],
			"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
	}))
	defer srv.Close()

	p := &OpenAI{BaseURL: srv.URL + "/v1/", APIKey: "sk-x", Client: srv.Client()}
	temp := 0.3
	resp, err := p.Chat(context.Background(), Request{
		Model: "m",
		Messages: []Message{
			{Role: RoleSystem, Content: "sys"},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c0", Name: "weather", Arguments: json.RawMessage(`{"city":"X"}`)}}},
			{Role: RoleTool, ToolCallID: "c0", Name: "weather", Content: "sunny"},
		},
		Tools:       []ToolDef{weatherTool()},
		Temperature: &temp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer sk-x" {
		t.Errorf("auth = %q", auth)
	}
	msgs := got["messages"].([]any)
	assistant := msgs[2].(map[string]any)
	if assistant["content"] != nil {
		t.Errorf("assistant tool-call content should be null, got %v", assistant["content"])
	}
	fn := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"city":"X"}` {
		t.Errorf("arguments must be sent as a string, got %v", fn["arguments"])
	}
	if got["temperature"] != 0.3 || got["stream"] != false || len(got["tools"].([]any)) != 1 {
		t.Errorf("unexpected body: %v", got)
	}

	if len(resp.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls = %d", len(resp.Message.ToolCalls))
	}
	if tc := resp.Message.ToolCalls[0]; tc.ID != "c1" || string(tc.Arguments) != `{"city":"Bangkok"}` {
		t.Errorf("call 0 = %+v", tc)
	}
	second := resp.Message.ToolCalls[1]
	if second.ID == "" {
		t.Error("missing id should be generated")
	}
	if raw, bad := InvalidArguments(second.Arguments); !bad || raw != "not json" {
		t.Errorf("invalid args not flagged: %s", second.Arguments)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestOllamaChat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"message":{"role":"assistant","content":"","tool_calls":[
			{"function":{"name":"weather","arguments":{"city":"Chiang Mai"}}}]},
			"prompt_eval_count":42,"eval_count":7}`)
	}))
	defer srv.Close()

	p := &Ollama{BaseURL: srv.URL, Client: srv.Client()}
	resp, err := p.Chat(context.Background(), Request{
		Model:       "qwen3",
		ContextSize: 8192,
		Messages: []Message{
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c0", Name: "weather", Arguments: json.RawMessage(`{"city":"X"}`)}}},
			{Role: RoleTool, ToolCallID: "c0", Name: "weather", Content: "sunny"},
		},
		Tools: []ToolDef{weatherTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts := got["options"].(map[string]any); opts["num_ctx"] != float64(8192) {
		t.Errorf("num_ctx missing: %v", got["options"])
	}
	msgs := got["messages"].([]any)
	args := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]
	if _, isObj := args.(map[string]any); !isObj {
		t.Errorf("arguments must be a JSON object, got %T", args)
	}
	if msgs[2].(map[string]any)["tool_name"] != "weather" {
		t.Errorf("tool_name missing: %v", msgs[2])
	}
	if len(resp.Message.ToolCalls) != 1 || string(resp.Message.ToolCalls[0].Arguments) != `{"city":"Chiang Mai"}` {
		t.Errorf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if resp.Message.ToolCalls[0].ID == "" || resp.Usage.PromptTokens != 42 {
		t.Errorf("id/usage wrong: %+v", resp)
	}
}

func TestOllamaNoOptionsWithoutContextSize(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"message":{"role":"assistant","content":"ok"}}`)
	}))
	defer srv.Close()
	p := &Ollama{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["options"]; ok {
		t.Errorf("options should be omitted: %v", got["options"])
	}
}

func TestToolsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"registry.ollama.ai/library/gemma does not support tools"}`)
	}))
	defer srv.Close()
	p := &Ollama{BaseURL: srv.URL, Client: srv.Client()}
	_, err := p.Chat(context.Background(), Request{Model: "m", Tools: []ToolDef{weatherTool()}, Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != ErrToolsUnsupported {
		t.Fatalf("err = %v, want ErrToolsUnsupported", err)
	}
}

func TestRetryOn5xxButNotOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Client: srv.Client()}
	resp, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil || resp.Message.Content != "hello" || calls.Load() != 3 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, calls.Load())
	}

	calls.Store(0)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
		io.WriteString(w, "nope")
	}))
	defer bad.Close()
	p = &OpenAI{BaseURL: bad.URL, Client: bad.Client()}
	_, err = p.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if ae, ok := err.(*APIError); !ok || ae.Status != 401 || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestEstimateTokens(t *testing.T) {
	if n := EstimateTokens("hello world, this is a test"); n < 5 || n > 12 {
		t.Errorf("ascii estimate = %d", n)
	}
	thai := "สวัสดีครับ วันนี้อากาศดีมาก"
	if EstimateTokens(thai) <= EstimateTokens("hello world, this is a test")/2 {
		t.Errorf("thai should cost more tokens per character")
	}
}
