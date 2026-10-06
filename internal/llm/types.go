// Package llm defines the provider-neutral chat types and the Provider
// interface, plus implementations for Ollama and OpenAI-compatible APIs.
package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// Role is the author of a chat message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a model request to run a tool. Arguments is always a JSON
// object (providers normalise their wire formats into this shape).
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message is one chat message. For RoleTool messages, ToolCallID links the
// result to the call and Name holds the tool name.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolDef describes a callable tool; Parameters is a JSON Schema object.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Request is a single chat completion request.
type Request struct {
	Model    string
	Messages []Message
	Tools    []ToolDef
	// ContextSize is the context window to request (Ollama num_ctx); 0 leaves
	// it to the server default.
	ContextSize int
	Temperature *float64
}

// Usage reports token counts as measured by the server (0 when unknown).
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Response is the model's reply.
type Response struct {
	Message Message
	Usage   Usage
}

// Provider talks to a chat model.
type Provider interface {
	Chat(ctx context.Context, req Request) (*Response, error)
}

// ErrToolsUnsupported is returned when the model rejects tool definitions.
var ErrToolsUnsupported = errors.New("model does not support tools")

// invalidArgsKey marks tool-call arguments that were not valid JSON.
const invalidArgsKey = "_invalid_arguments"

// InvalidArguments reports whether args were flagged as unparseable by a
// provider, returning the original text.
func InvalidArguments(args json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(args, &m) != nil || len(m) != 1 {
		return "", false
	}
	raw, ok := m[invalidArgsKey]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// normalizeArgs turns provider-supplied tool arguments into a JSON object.
func normalizeArgs(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &obj) == nil && obj != nil {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(map[string]string{invalidArgsKey: s})
	return b
}

// NewCallID returns a random tool-call identifier.
func NewCallID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "call_" + hex.EncodeToString(b[:])
}

// EstimateTokens roughly estimates the token count of s. It is deliberately
// script-aware: ASCII text averages ~4 chars/token while Thai/CJK text costs
// far more tokens per character.
func EstimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + (other*3+2)/4 + 1
}

// EstimateMessages estimates the token count of a message list, including a
// small per-message overhead.
func EstimateMessages(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += 4 + EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			n += EstimateTokens(tc.Name) + EstimateTokens(string(tc.Arguments))
		}
	}
	return n
}

// truncateRunes shortens s to at most max runes.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}
