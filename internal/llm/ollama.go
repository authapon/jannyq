package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Ollama talks to Ollama's native /api/chat endpoint. The native API is used
// (rather than /v1) because it is the only one that honours num_ctx.
type Ollama struct {
	BaseURL string // e.g. http://localhost:11434
	APIKey  string // optional bearer token for proxied/cloud setups
	Client  *http.Client
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    [][]byte         `json:"images,omitempty"` // base64 in JSON
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type ollamaResponse struct {
	Message         ollamaMessage `json:"message"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	Error           string        `json:"error"`
}

// Chat implements Provider.
func (o *Ollama) Chat(ctx context.Context, req Request) (*Response, error) {
	body := ollamaRequest{Model: req.Model, Stream: false}
	for _, m := range req.Messages {
		om := ollamaMessage{Role: string(m.Role), Content: m.Content}
		for _, img := range m.Images {
			om.Images = append(om.Images, img.Data)
		}
		if m.Role == RoleTool {
			om.ToolName = m.Name
		}
		for _, tc := range m.ToolCalls {
			var otc ollamaToolCall
			otc.ID = tc.ID
			otc.Function.Name = tc.Name
			otc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, otc)
		}
		body.Messages = append(body.Messages, om)
	}
	for _, t := range req.Tools {
		var ot ollamaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		body.Tools = append(body.Tools, ot)
	}
	opts := map[string]any{}
	if req.ContextSize > 0 {
		opts["num_ctx"] = req.ContextSize
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if len(opts) > 0 {
		body.Options = opts
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	var resp ollamaResponse
	url := strings.TrimRight(o.BaseURL, "/") + "/api/chat"
	if err := postJSON(ctx, o.Client, url, headers, body, &resp); err != nil {
		var ae *APIError
		if asAPIError(err, &ae) && len(req.Tools) > 0 && ae.Status == http.StatusBadRequest &&
			strings.Contains(strings.ToLower(ae.Body), "does not support tools") {
			return nil, ErrToolsUnsupported
		}
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New("llm: " + resp.Error)
	}
	out := Message{Role: RoleAssistant, Content: resp.Message.Content}
	for _, tc := range resp.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = NewCallID()
		}
		args := string(tc.Function.Arguments)
		// Some models emit arguments as a JSON-encoded string.
		var asString string
		if json.Unmarshal(tc.Function.Arguments, &asString) == nil {
			args = asString
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: normalizeArgs(args),
		})
	}
	return &Response{
		Message: out,
		Usage:   Usage{PromptTokens: resp.PromptEvalCount, CompletionTokens: resp.EvalCount},
	}, nil
}

// SupportsVision asks Ollama whether the model can look at pictures, using
// /api/show: newer servers list "vision" among the model's capabilities, older
// ones show a CLIP projector in the model details.
func (o *Ollama) SupportsVision(ctx context.Context, model string) (bool, error) {
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	var resp struct {
		Capabilities []string `json:"capabilities"`
		Details      struct {
			Families []string `json:"families"`
		} `json:"details"`
		ModelInfo map[string]json.RawMessage `json:"model_info"`
	}
	url := strings.TrimRight(o.BaseURL, "/") + "/api/show"
	if err := postJSON(ctx, o.Client, url, headers, map[string]string{"model": model}, &resp); err != nil {
		return false, err
	}
	for _, c := range resp.Capabilities {
		if strings.EqualFold(c, "vision") {
			return true, nil
		}
	}
	if len(resp.Capabilities) > 0 {
		return false, nil // the server lists capabilities, and vision is not among them
	}
	for _, f := range resp.Details.Families {
		if strings.EqualFold(f, "clip") || strings.EqualFold(f, "mllama") {
			return true, nil
		}
	}
	for k := range resp.ModelInfo {
		if strings.Contains(k, ".vision.") || strings.HasPrefix(k, "clip.") {
			return true, nil
		}
	}
	return false, nil
}
