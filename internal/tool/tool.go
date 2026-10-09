// Package tool defines the tools the model can call and a registry for them.
package tool

import (
	"context"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/authapon/jannyq/internal/llm"
)

// CallContext identifies who and where a tool call originates from.
type CallContext struct {
	SessionKey string
	// ChatID and IsGroup say which chat the call comes from.
	ChatID   string
	IsGroup  bool
	Channel  string
	UserID   string
	UserName string
	// Origin is set for anonymous channels (the web chat): the address the
	// user comes from, which limits should be keyed on instead of the user.
	Origin string
	Lang   string
	// Attachments gives access to the files sent in this chat; nil when the
	// chat has none or the feature is off.
	Attachments AttachmentSource
}

// Tool is a function the model may call.
type Tool interface {
	Name() string
	Description() string
	// Parameters returns the JSON Schema of the arguments object.
	Parameters() []byte
	// Execute runs the tool. A returned error is reported back to the model
	// as the tool result so it can recover or explain.
	Execute(ctx context.Context, cc CallContext, args []byte) (string, error)
}

// Hinter is implemented by tools that add guidance to the system prompt.
type Hinter interface {
	Hint() string
}

// Uncapped is implemented by tools whose results are not cut at
// --tool-max-output because they have bounded their own size.
type Uncapped interface {
	Uncapped() bool
}

// Registry holds the enabled tools.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

// Register adds a tool, replacing any tool with the same name.
func (r *Registry) Register(t Tool) { r.tools[t.Name()] = t }

// Get looks a tool up by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Len returns the number of registered tools.
func (r *Registry) Len() int { return len(r.tools) }

// Defs returns the tool definitions in a stable order.
func (r *Registry) Defs() []llm.ToolDef {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	defs := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		defs = append(defs, llm.ToolDef{Name: n, Description: t.Description(), Parameters: t.Parameters()})
	}
	return defs
}

// Hints returns the prompt guidance of all registered tools, in name order.
func (r *Registry) Hints() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if h, ok := r.tools[n].(Hinter); ok {
			if hint := h.Hint(); hint != "" {
				out = append(out, hint)
			}
		}
	}
	return out
}

// Truncate shortens s to at most max runes and notes how much was dropped.
func Truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + fmt.Sprintf("\n[truncated: %d more characters]", len(r)-max)
}
