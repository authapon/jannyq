package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/authapon/jannyq/internal/i18n"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
)

// Introduce has the model introduce itself in a chat it has not spoken in
// before. The model is given the same system prompt as for any answer, so what
// it says about itself (its name, tools, skills, file handling) follows the
// setup, and it writes in the main language. The introduction is stored in the
// history as an ordinary assistant message, after the message that prompted
// it, and returned. extra holds facts only the caller knows, such as the chat
// commands that exist.
func (a *Agent) Introduce(ctx context.Context, s *session.Session, in Input, extra string) (string, error) {
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
	key := s.Channel + ":" + s.ChatID
	history := a.renderHistory(key, in.IsGroup, stored, view)
	msgs := a.buildMessages(in, summary, history, a.tools.Len() > 0, false)

	var sb strings.Builder
	fmt.Fprintf(&sb, "Note from the system (not from a user): this is the first time you speak in this chat. "+
		"Before you answer anything, introduce yourself in a short message written in %s: say who you are "+
		"and what you can help with, going only by what this prompt says you can do (tools, skills, files); "+
		"do not claim abilities that are not listed. ", i18n.LanguageName(a.cfg.Lang))
	if extra = strings.TrimSpace(extra); extra != "" {
		sb.WriteString(extra)
		sb.WriteString(" ")
	}
	sb.WriteString("Be warm and brief: two to four short sentences, no headings or tables. " +
		"Do not answer the user's message yet, do not call tools, and do not mention this note.")
	msgs = append(msgs, llm.Message{Role: llm.RoleSystem, Content: sb.String()})

	resp, err := a.chat(ctx, llm.Request{
		Model:       a.cfg.Model,
		Messages:    msgs,
		ContextSize: a.cfg.ContextSize,
		Temperature: a.cfg.Temperature,
	})
	if err != nil {
		return "", err
	}
	text := cleanReply(resp.Message.Content)
	if text == "" {
		return "", ErrEmptyResponse
	}
	if err := s.Append(ctx, llm.Message{Role: llm.RoleAssistant, Content: text}); err != nil {
		return "", fmt.Errorf("store introduction: %w", err)
	}
	return text, nil
}

const afterIntroNote = "Note from the system (not from a user): your last message in this conversation was your " +
	"introduction. Now answer the user's message that came before it, without introducing yourself again."
