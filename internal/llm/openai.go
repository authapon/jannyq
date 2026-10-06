package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// OpenAI talks to any OpenAI-compatible /chat/completions endpoint
// (OpenAI, vLLM, LM Studio, llama.cpp, Ollama's /v1 API, ...).
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Client  *http.Client
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

type oaiPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream"`
}

type oaiResponse struct {
	Choices []struct {
		Message struct {
			Content   *string       `json:"content"`
			ToolCalls []oaiToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func toOAIMessages(msgs []Message) []oaiMessage {
	out := make([]oaiMessage, 0, len(msgs))
	for _, m := range msgs {
		om := oaiMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if m.Role == RoleTool {
			om.Name = m.Name
		}
		switch {
		case m.Role == RoleAssistant && len(m.ToolCalls) > 0 && m.Content == "":
			om.Content = nil
		case len(m.Images) > 0:
			// vision input: a list of parts, the text first
			parts := make([]oaiPart, 0, len(m.Images)+1)
			if m.Content != "" {
				parts = append(parts, oaiPart{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				parts = append(parts, oaiPart{Type: "image_url", ImageURL: &oaiImageURL{
					URL: "data:" + img.MIME + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
				}})
			}
			om.Content = parts
		default:
			om.Content = m.Content
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, oaiToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: oaiFunction{Name: tc.Name, Arguments: string(tc.Arguments)},
			})
		}
		out = append(out, om)
	}
	return out
}

// Chat implements Provider.
func (o *OpenAI) Chat(ctx context.Context, req Request) (*Response, error) {
	body := oaiRequest{
		Model:       req.Model,
		Messages:    toOAIMessages(req.Messages),
		Temperature: req.Temperature,
	}
	for _, t := range req.Tools {
		var ot oaiTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.Parameters
		body.Tools = append(body.Tools, ot)
	}
	headers := map[string]string{}
	if o.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.APIKey
	}
	var resp oaiResponse
	url := strings.TrimRight(o.BaseURL, "/") + "/chat/completions"
	if err := postJSON(ctx, o.Client, url, headers, body, &resp); err != nil {
		var ae *APIError
		if asAPIError(err, &ae) && len(req.Tools) > 0 && ae.Status == http.StatusBadRequest &&
			strings.Contains(strings.ToLower(ae.Body), "does not support tools") {
			return nil, ErrToolsUnsupported
		}
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("llm: response has no choices")
	}
	msg := resp.Choices[0].Message
	out := Message{Role: RoleAssistant}
	if msg.Content != nil {
		out.Content = *msg.Content
	}
	for _, tc := range msg.ToolCalls {
		id := tc.ID
		if id == "" {
			id = NewCallID()
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: normalizeArgs(tc.Function.Arguments),
		})
	}
	return &Response{
		Message: out,
		Usage:   Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens},
	}, nil
}
