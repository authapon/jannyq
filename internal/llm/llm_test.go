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

func TestOpenAISendsImagesAsDataURIs(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"choices":[{"message":{"content":"a cat"}}]}`)
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Client: srv.Client()}
	_, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "what is this?", Images: []Image{{MIME: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff}}, {MIME: "image/png", Data: []byte("PNG")}}},
		{Role: RoleUser, Content: "plain"},
		{Role: RoleUser, Images: []Image{{MIME: "image/png", Data: []byte("X")}}}, // image without text
	}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "sys" || msgs[2].(map[string]any)["content"] != "plain" {
		t.Errorf("text-only messages must keep a plain string: %v", msgs)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	if p0 := parts[0].(map[string]any); p0["type"] != "text" || p0["text"] != "what is this?" {
		t.Errorf("first part = %v", p0)
	}
	u1 := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	u2 := parts[2].(map[string]any)["image_url"].(map[string]any)["url"]
	if u1 != "data:image/jpeg;base64,/9j/" || u2 != "data:image/png;base64,UE5H" {
		t.Errorf("urls = %v, %v", u1, u2)
	}
	only := msgs[3].(map[string]any)["content"].([]any)
	if len(only) != 1 || only[0].(map[string]any)["type"] != "image_url" {
		t.Errorf("an image-only message must have no empty text part: %v", only)
	}
}

func TestOllamaSendsImagesAsBase64(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		io.WriteString(w, `{"message":{"role":"assistant","content":"a dog"}}`)
	}))
	defer srv.Close()
	p := &Ollama{BaseURL: srv.URL, Client: srv.Client()}
	_, err := p.Chat(context.Background(), Request{Model: "m", Messages: []Message{
		{Role: RoleUser, Content: "look", Images: []Image{{MIME: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff}}}},
		{Role: RoleUser, Content: "no image"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	imgs := msgs[0].(map[string]any)["images"].([]any)
	if len(imgs) != 1 || imgs[0] != "/9j/" {
		t.Errorf("images = %v", imgs)
	}
	if _, has := msgs[1].(map[string]any)["images"]; has {
		t.Error("a message without images must not carry an images field")
	}
}

func TestImagesCountInTheTokenEstimate(t *testing.T) {
	plain := EstimateMessages([]Message{{Role: RoleUser, Content: "hello"}})
	with := EstimateMessages([]Message{{Role: RoleUser, Content: "hello", Images: []Image{{}, {}}}})
	if with-plain != 2*ImageTokens {
		t.Errorf("two images add %d tokens, want %d", with-plain, 2*ImageTokens)
	}
}

func TestOllamaSupportsVision(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want bool
	}{
		"capabilities with vision":    {`{"capabilities":["completion","vision","tools"]}`, true},
		"capabilities without vision": {`{"capabilities":["completion","tools"],"details":{"families":["clip"]}}`, false},
		"old server, clip family":     {`{"details":{"families":["llama","clip"]}}`, true},
		"old server, vision keys":     {`{"model_info":{"general.architecture":"x","gemma3.vision.block_count":27}}`, true},
		"old server, text only":       {`{"details":{"families":["llama"]},"model_info":{"llama.block_count":32}}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if r.URL.Path != "/api/show" || in["model"] != "m1" || r.Header.Get("Authorization") != "Bearer k" {
				t.Errorf("%s: request %s %v %q", name, r.URL.Path, in, r.Header.Get("Authorization"))
			}
			io.WriteString(w, tc.body)
		}))
		o := &Ollama{BaseURL: srv.URL, APIKey: "k", Client: srv.Client()}
		got, err := o.SupportsVision(context.Background(), "m1")
		srv.Close()
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"error":"model not found"}`)
	}))
	defer srv.Close()
	o := &Ollama{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := o.SupportsVision(context.Background(), "nope"); err == nil {
		t.Error("an unknown model must be an error, not 'no vision'")
	}
}
