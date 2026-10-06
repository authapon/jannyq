package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/authapon/jannyq/internal/i18n"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/tool"
)

const (
	// keptTokenShare caps how much of the context the verbatim tail may use
	// after compaction, so the next turns do not immediately re-trigger it.
	keptTokenShare = 0.4
	// promptOverhead approximates the system prompt and tool definitions.
	promptOverhead = 800

	maxSummaryRunes   = 8000
	excerptMsgRunes   = 3000 // per user/assistant message in the summariser input
	excerptToolRunes  = 600  // per tool result
	defaultBatchToken = 6000
)

// MaybeCompact summarises old messages when the conversation is too long,
// either by message count or by estimated prompt size.
func (a *Agent) MaybeCompact(ctx context.Context, s *session.Session) (bool, error) {
	count, err := s.Count(ctx)
	if err != nil {
		return false, err
	}
	need := count > a.cfg.CompactAfter
	if !need && a.cfg.ContextSize > 0 {
		summary, err := s.Summary(ctx)
		if err != nil {
			return false, err
		}
		stored, err := s.Messages(ctx)
		if err != nil {
			return false, err
		}
		view, err := a.newView(ctx, s, stored, false)
		if err != nil {
			return false, err
		}
		est := llm.EstimateTokens(summary) + promptOverhead + estimateStored(stored) + view.tokens(stored)
		if last := s.LastPromptTokens(ctx); last > est {
			est = last
		}
		need = float64(est) > a.cfg.CompactRatio*float64(a.cfg.ContextSize)
	}
	if !need {
		return false, nil
	}
	return a.Compact(ctx, s, a.cfg.CompactKeep)
}

// Compact summarises everything except roughly the last keep messages
// (always cutting at a user message). It reports whether anything changed.
func (a *Agent) Compact(ctx context.Context, s *session.Session, keep int) (bool, error) {
	stored, err := s.Messages(ctx)
	if err != nil {
		return false, err
	}
	view, err := a.newView(ctx, s, stored, false)
	if err != nil {
		return false, err
	}
	cut := a.chooseCut(stored, keep, view)
	if cut <= 0 {
		return false, nil
	}
	old := stored[:cut]
	prev, err := s.Summary(ctx)
	if err != nil {
		return false, err
	}
	// The summariser sees the messages exactly as the model does, with who said
	// them and when, so that the summary can keep that.
	briefView, err := a.newView(ctx, s, old, true)
	if err != nil {
		return false, err
	}
	rendered := a.renderHistory(s.Channel+":"+s.ChatID, s.IsGroup(ctx), old, briefView)
	summary, err := a.summarize(ctx, prev, rendered)
	if err != nil {
		return false, err
	}
	if err := s.Compact(ctx, old[len(old)-1].ID, summary); err != nil {
		return false, err
	}
	a.log.Info("conversation compacted", "chat", s.Channel+":"+s.ChatID,
		"summarised", len(old), "kept", len(stored)-cut)
	return true, nil
}

// chooseCut returns the index of the first message to keep verbatim: a user
// message such that at most keep messages (and, when the context size is
// known, a bounded number of tokens) remain. 0 means nothing can be compacted.
func (a *Agent) chooseCut(stored []session.Stored, keep int, v *attachView) int {
	var users []int
	for i, st := range stored {
		if st.Message.Role == llm.RoleUser {
			users = append(users, i)
		}
	}
	for n, i := range users {
		isLast := n == len(users)-1
		if len(stored)-i > keep && !isLast {
			continue
		}
		if a.cfg.ContextSize > 0 && !isLast &&
			float64(estimateStored(stored[i:])+v.tokens(stored[i:])) > keptTokenShare*float64(a.cfg.ContextSize) {
			continue
		}
		return i
	}
	return 0
}

func estimateStored(stored []session.Stored) int {
	n := 0
	for _, st := range stored {
		n += llm.EstimateMessages([]llm.Message{st.Message})
		if st.Message.Role == llm.RoleUser {
			n += headerTokens
		}
	}
	return n
}

// summarize folds old messages into the running summary, in batches small
// enough for the model's context window.
func (a *Agent) summarize(ctx context.Context, summary string, old []llm.Message) (string, error) {
	budget := defaultBatchToken
	if a.cfg.ContextSize > 0 {
		if b := int(0.5*float64(a.cfg.ContextSize)) - 1500; b > 1000 {
			budget = b
		} else {
			budget = 1000
		}
	}
	var batch []string
	tokens := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		next, err := a.summarizeBatch(ctx, summary, strings.Join(batch, "\n"))
		if err != nil {
			return err
		}
		summary = next
		batch, tokens = nil, 0
		return nil
	}
	for _, m := range old {
		line := excerptLine(m)
		if line == "" {
			continue
		}
		t := llm.EstimateTokens(line)
		if tokens+t > budget && len(batch) > 0 {
			if err := flush(); err != nil {
				return "", err
			}
		}
		batch = append(batch, line)
		tokens += t
	}
	if err := flush(); err != nil {
		return "", err
	}
	return summary, nil
}

func excerptLine(m llm.Message) string {
	switch m.Role {
	case llm.RoleUser:
		return "User: " + oneLine(tool.Truncate(m.Content, excerptMsgRunes))
	case llm.RoleAssistant:
		var parts []string
		for _, tc := range m.ToolCalls {
			parts = append(parts, fmt.Sprintf("[assistant called %s %s]", tc.Name, tool.Truncate(string(tc.Arguments), 200)))
		}
		if m.Content != "" {
			parts = append(parts, "Assistant: "+oneLine(tool.Truncate(m.Content, excerptMsgRunes)))
		}
		return strings.Join(parts, "\n")
	case llm.RoleTool:
		return fmt.Sprintf("[%s result: %s]", m.Name, oneLine(tool.Truncate(m.Content, excerptToolRunes)))
	}
	return ""
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func (a *Agent) summarizeBatch(ctx context.Context, summary, excerpt string) (string, error) {
	if summary == "" {
		summary = "(none yet)"
	}
	system := "You maintain the running summary of a chat between one or more users and an AI assistant. " +
		"Update the summary so that it also covers the new conversation excerpt. " +
		"Keep: who said or decided what (use the speakers' names) and on which date (YYYY-MM-DD, taken from the " +
		"message headers), facts about the people, their preferences, decisions made, open tasks or questions, " +
		"and important details (numbers, names, URLs, commands, file names) plus the conclusions of any searches. " +
		"Drop greetings and redundant detail. Write in " + i18n.LanguageName(a.cfg.Lang) + ". " +
		"Output only the updated summary as concise bullet points, at most about 400 words."
	user := "Current summary:\n" + summary + "\n\nNew conversation excerpt:\n" + excerpt + "\n\nUpdated summary:"
	resp, err := a.provider.Chat(ctx, llm.Request{
		Model:       a.cfg.Model,
		Messages:    []llm.Message{{Role: llm.RoleSystem, Content: system}, {Role: llm.RoleUser, Content: user}},
		ContextSize: a.cfg.ContextSize,
	})
	if err != nil {
		return "", err
	}
	out := cleanReply(resp.Message.Content)
	if out == "" {
		return "", ErrEmptyResponse
	}
	r := []rune(out)
	if len(r) > maxSummaryRunes {
		out = string(r[:maxSummaryRunes])
	}
	return out, nil
}
