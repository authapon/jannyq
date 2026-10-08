// Package agent runs the conversation loop: it feeds a chat's history to the
// model, executes the tool calls the model asks for, and keeps the history
// within the model's context window by summarising old messages.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/authapon/jannyq/internal/metrics"
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

	// Location is the time zone in which message times are recorded and
	// shown; nil means the server's local time. TimezoneName is how the
	// system prompt names it (default: the location's name).
	Location     *time.Location
	TimezoneName string

	// Vision says whether the model can look at pictures. ImageMessages is how
	// many of the latest messages with pictures are sent with their pictures
	// (default 3); InlineChars is the longest attachment text shown in the
	// conversation itself (default 6000).
	Vision        bool
	ImageMessages int
	InlineChars   int
	// Attachments says that users may send files; it adds guidance to the
	// system prompt. The read_attachment and search_attachment tools should be
	// registered as well.
	Attachments bool
	// Inbox says that copies of attachments are placed in the command
	// workspace (see attach.InboxPath) when run_command is available.
	Inbox bool

	// Skills, when set, lists skills in the system prompt (the load_skill
	// tool must be registered for the model to use them).
	Skills SkillCatalog

	// Metrics counts model requests, tokens, tool calls and compactions.
	Metrics metrics.Instruments
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
	// SentAt is when the message was sent; the zero value means "now".
	SentAt time.Time
	// AnswerFor is the id returned by Record of the message being answered.
	// When later messages were stored while it waited, the model is told which
	// one to answer. Zero means the newest.
	AnswerFor int64
	// Introduced says that the model has just introduced itself in reply to
	// this message, so the answer should not repeat it.
	Introduced bool
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
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.TimezoneName == "" {
		cfg.TimezoneName = cfg.Location.String()
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

// storeUser saves an incoming message together with who sent it and when, and
// returns its id. How sender and time are shown to the model is decided later,
// when the prompt is built.
func (a *Agent) storeUser(ctx context.Context, s *session.Session, in Input) (int64, error) {
	sent := in.SentAt
	if sent.IsZero() {
		sent = a.now()
	}
	name := strings.TrimSpace(in.Sender)
	if rs := []rune(name); len(rs) > 100 {
		name = string(rs[:100])
	}
	if s.IsGroup(ctx) != in.IsGroup {
		if err := s.SetGroup(ctx, in.IsGroup); err != nil {
			return 0, err
		}
	}
	return s.AppendEntry(ctx, session.Entry{
		Message:    llm.Message{Role: llm.RoleUser, Content: strings.TrimSpace(in.Text)},
		SenderID:   in.UserID,
		SenderName: name,
		SentAt:     FormatTime(sent, a.cfg.Location),
	})
}

// Record stores a message without answering it and returns its id. It is how
// messages are taken in, in the order they were sent, and how group
// conversations that nobody asked the bot to answer are kept for later.
func (a *Agent) Record(ctx context.Context, s *session.Session, in Input) (int64, error) {
	id, err := a.storeUser(ctx, s, in)
	if err != nil {
		return 0, fmt.Errorf("store message: %w", err)
	}
	return id, nil
}

// Reply stores the user's message and answers it: Record followed by Respond.
func (a *Agent) Reply(ctx context.Context, s *session.Session, in Input) (string, error) {
	id, err := a.Record(ctx, s, in)
	if err != nil {
		return "", err
	}
	in.AnswerFor = id
	return a.Respond(ctx, s, in)
}

// Respond runs the model (and any tools it calls) over a conversation whose
// latest user message has already been stored, and returns the final answer,
// which is also stored in the history.
func (a *Agent) Respond(ctx context.Context, s *session.Session, in Input) (string, error) {
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
	if a.cfg.Attachments {
		cc.Attachments = sessionFiles{s}
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
		view, err := a.newView(ctx, s, stored, false)
		if err != nil {
			return "", err
		}
		history := a.renderHistory(cc.SessionKey, in.IsGroup, stored, view)
		note := a.answerNote(cc.SessionKey, in.IsGroup, stored, in.AnswerFor)

		var defs []llm.ToolDef
		final := step == a.cfg.MaxSteps // out of tool rounds: force an answer
		if !final && !a.toolsUnsupported.Load() {
			defs = a.tools.Defs()
		}
		msgs := a.buildMessages(in, summary, history, len(defs) > 0, final)
		if note != "" {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: note})
		}
		if in.Introduced {
			msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: afterIntroNote})
		}

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
	sys := a.systemPrompt(in, hasTools)
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
	start := time.Now()
	resp, err := a.provider.Chat(ctx, req)
	if errors.Is(err, llm.ErrToolsUnsupported) && len(req.Tools) > 0 {
		if !a.toolsUnsupported.Swap(true) {
			a.log.Warn("model does not support tools; continuing without them", "model", req.Model)
		}
		req.Tools = nil
		resp, err = a.provider.Chat(ctx, req)
	}
	a.cfg.Metrics.LLMSeconds.Since(start)
	if err != nil {
		a.cfg.Metrics.LLMRequests.Inc("error")
		return resp, err
	}
	a.cfg.Metrics.LLMRequests.Inc("ok")
	a.cfg.Metrics.LLMTokens.Add(float64(resp.Usage.PromptTokens), "prompt")
	a.cfg.Metrics.LLMTokens.Add(float64(resp.Usage.CompletionTokens), "completion")
	return resp, nil
}

// runTool executes one tool call and returns the text to give the model.
func (a *Agent) runTool(ctx context.Context, cc tool.CallContext, tc llm.ToolCall, index int) (result string) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			result = fmt.Sprintf("Error: tool %s crashed: %v", tc.Name, r)
		}
		name, outcome := tc.Name, "ok"
		if _, known := a.tools.Get(name); !known {
			name = "unknown" // the model made the name up: do not let it create series
		}
		if strings.HasPrefix(result, "Error: ") {
			outcome = "error"
		}
		a.cfg.Metrics.ToolCalls.Inc(name, outcome)
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

// answerNote tells the model which message to answer when messages were
// stored after it, which happens when the chat was busy or people kept
// talking. It goes at the very end of the prompt, so it does not disturb the
// part of the conversation the model server has already cached. When the
// message to answer is the newest, there is nothing to say.
func (a *Agent) answerNote(chatKey string, group bool, stored []session.Stored, answerFor int64) string {
	if answerFor == 0 {
		return ""
	}
	idx, later := -1, false
	for i, st := range stored {
		if st.ID == answerFor {
			idx = i
		} else if idx >= 0 && st.Message.Role == llm.RoleUser {
			later = true
		}
	}
	if idx < 0 || !later {
		return ""
	}
	st := stored[idx]
	quote := strings.Join(strings.Fields(neutralizeHeaders(st.Message.Content)), " ")
	if rs := []rune(quote); len(rs) > 160 {
		quote = string(rs[:160]) + "…"
	}
	return fmt.Sprintf("Note from the system (not from a user): you are now answering the message %s %q. "+
		"The messages after it were sent while it was waiting and will be answered separately; "+
		"use them as context but do not answer them now.", a.header(chatKey, group, st), quote)
}
