// Package agent runs the conversation loop: it feeds a chat's history to the
// model, executes the tool calls the model asks for, and keeps the history
// within the model's context window by summarising old messages.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/skill"
	"github.com/authapon/jannyq/internal/tool"
)

// ErrEmptyResponse is returned when the model produced no usable text.
var ErrEmptyResponse = errors.New("agent: empty model response")

// maxToolCallsPerStep bounds how many tool calls one model turn may request.
const maxToolCallsPerStep = 8

// Config tunes the agent.
type Config struct {
	Model       string
	ContextSize int // 0 = unknown
	Temperature *float64
	MaxSteps    int // max tool rounds per user message

	Lang        string
	LangMode    string
	BotName     string
	ExtraPrompt string

	CompactAfter int     // compact when conversational messages exceed this
	CompactRatio float64 // ...or when the prompt exceeds this share of ContextSize
	CompactKeep  int     // recent messages kept verbatim

	ToolMaxOutput int // max characters of one tool result kept in history

	// Skills, when set, lists skills in the system prompt (the load_skill
	// tool must be registered for the model to use them).
	Skills SkillCatalog
}

// SkillCatalog provides the skill list shown to the model.
type SkillCatalog interface {
	Summaries() []skill.Summary
}

// Input is one incoming user message.
type Input struct {
	Text    string
	Sender  string // display name; prefixed to the text in group chats
	IsGroup bool
	Channel string
	UserID  string
	Origin  string // see channel.Incoming.Origin
}

// Agent drives conversations.
type Agent struct {
	cfg      Config
	provider llm.Provider
	tools    *tool.Registry
	log      *slog.Logger
	now      func() time.Time

	toolsUnsupported atomic.Bool
}

// New creates an Agent.
func New(cfg Config, p llm.Provider, tools *tool.Registry, log *slog.Logger) *Agent {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	if cfg.CompactAfter <= 0 {
		cfg.CompactAfter = 200
	}
	if cfg.CompactRatio <= 0 || cfg.CompactRatio > 1 {
		cfg.CompactRatio = 0.75
	}
	if cfg.CompactKeep <= 0 {
		cfg.CompactKeep = 20
	}
	if cfg.ToolMaxOutput <= 0 {
		cfg.ToolMaxOutput = 12000
	}
	if tools == nil {
		tools = tool.NewRegistry()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Agent{cfg: cfg, provider: p, tools: tools, log: log, now: time.Now}
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// cleanReply removes reasoning blocks some models inline into their answer.
func cleanReply(s string) string {
	return strings.TrimSpace(thinkRe.ReplaceAllString(s, ""))
}

// Reply records the user's message, runs the model (and any tools it calls)
// and returns the final answer, which is also stored in the history.
func (a *Agent) Reply(ctx context.Context, s *session.Session, in Input) (string, error) {
	text := strings.TrimSpace(in.Text)
	if in.IsGroup && in.Sender != "" {
		text = in.Sender + ": " + text
	}
	if err := s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: text}); err != nil {
		return "", fmt.Errorf("store message: %w", err)
	}
	// Make room first; failing to compact must not block answering.
	if _, err := a.MaybeCompact(ctx, s); err != nil {
		a.log.Warn("compaction failed", "chat", s.Channel+":"+s.ChatID, "err", err)
	}

	cc := tool.CallContext{
		SessionKey: s.Channel + ":" + s.ChatID,
		Channel:    in.Channel,
		UserID:     in.UserID,
		UserName:   in.Sender,
		Origin:     in.Origin,
		Lang:       a.cfg.Lang,
	}

	for step := 0; step <= a.cfg.MaxSteps; step++ {
		summary, err := s.Summary(ctx)
		if err != nil {
			return "", err
		}
		stored, err := s.Messages(ctx)
		if err != nil {
			return "", err
		}
		history := make([]llm.Message, 0, len(stored))
		for _, st := range stored {
			history = append(history, st.Message)
		}

		var defs []llm.ToolDef
		final := step == a.cfg.MaxSteps // out of tool rounds: force an answer
		if !final && !a.toolsUnsupported.Load() {
			defs = a.tools.Defs()
		}
		msgs := a.buildMessages(in, summary, history, len(defs) > 0, final)

		resp, err := a.chat(ctx, llm.Request{
			Model:       a.cfg.Model,
			Messages:    msgs,
			Tools:       defs,
			ContextSize: a.cfg.ContextSize,
			Temperature: a.cfg.Temperature,
		})
		if err != nil {
			return "", err
		}
		if resp.Usage.PromptTokens > 0 {
			_ = s.SetLastPromptTokens(ctx, resp.Usage.PromptTokens)
		}

		reply := resp.Message
		if len(reply.ToolCalls) == 0 || final {
			content := cleanReply(reply.Content)
			if content == "" {
				return "", ErrEmptyResponse
			}
			if err := s.Append(ctx, llm.Message{Role: llm.RoleAssistant, Content: content}); err != nil {
				return "", fmt.Errorf("store reply: %w", err)
			}
			return content, nil
		}

		// Persist the tool-call message together with its results so the
		// history never holds a call without an answer.
		reply.Content = cleanReply(reply.Content)
		batch := []llm.Message{reply}
		for i, tc := range reply.ToolCalls {
			result := a.runTool(ctx, cc, tc, i)
			batch = append(batch, llm.Message{
				Role:       llm.RoleTool,
				Name:       tc.Name,
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
		if err := s.Append(ctx, batch...); err != nil {
			return "", fmt.Errorf("store tool results: %w", err)
		}
	}
	return "", ErrEmptyResponse // unreachable: the final step always returns
}

// buildMessages assembles the model input: system prompt (with the summary
// of compacted history) followed by the stored messages.
func (a *Agent) buildMessages(in Input, summary string, history []llm.Message, hasTools, final bool) []llm.Message {
	sys := a.systemPrompt(in, hasTools, a.now())
	if summary != "" {
		sys += "\nSummary of the earlier conversation (older messages were condensed):\n" + summary + "\n"
	}
	if final {
		sys += "\nYou have used all available tool calls. Answer now using the information gathered so far.\n"
	}
	msgs := make([]llm.Message, 0, len(history)+1)
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: sys})
	return append(msgs, history...)
}

// chat calls the provider, retrying once without tools if the model turns
// out not to support them (and remembering that for later calls).
func (a *Agent) chat(ctx context.Context, req llm.Request) (*llm.Response, error) {
	resp, err := a.provider.Chat(ctx, req)
	if errors.Is(err, llm.ErrToolsUnsupported) && len(req.Tools) > 0 {
		if !a.toolsUnsupported.Swap(true) {
			a.log.Warn("model does not support tools; continuing without them", "model", req.Model)
		}
		req.Tools = nil
		return a.provider.Chat(ctx, req)
	}
	return resp, err
}

// runTool executes one tool call and returns the text to give the model.
func (a *Agent) runTool(ctx context.Context, cc tool.CallContext, tc llm.ToolCall, index int) (result string) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			result = fmt.Sprintf("Error: tool %s crashed: %v", tc.Name, r)
		}
		a.log.Info("tool call", "tool", tc.Name, "chat", cc.SessionKey, "user", cc.UserID,
			"duration", time.Since(start).Round(time.Millisecond), "bytes", len(result))
	}()

	if index >= maxToolCallsPerStep {
		return "Error: too many tool calls in one step; this call was skipped."
	}
	t, ok := a.tools.Get(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q.", tc.Name)
	}
	if raw, bad := llm.InvalidArguments(tc.Arguments); bad {
		return "Error: the tool arguments were not valid JSON: " + tool.Truncate(raw, 200)
	}
	if !json.Valid(tc.Arguments) {
		return "Error: the tool arguments were not valid JSON."
	}
	out, err := t.Execute(ctx, cc, tc.Arguments)
	if err != nil {
		return "Error: " + err.Error()
	}
	return tool.Truncate(out, a.cfg.ToolMaxOutput)
}
