package session

import "github.com/authapon/jannyq/internal/llm"

// Stored is a persisted message with its row ID. For user messages the
// sender and the time the message was sent are kept next to the text, so the
// way they are shown to the model can change without touching stored data.
type Stored struct {
	ID         int64
	Message    llm.Message
	SenderID   string // platform user id; empty for assistant and tool messages
	SenderName string
	// SentAt is when the message was sent, as ISO 8601 with the UTC offset
	// that applied then, e.g. 1997-07-16T19:20:44+01:00. It is empty for
	// messages stored before this was recorded; CreatedAt is then the fallback.
	SentAt    string
	CreatedAt int64 // Unix seconds at which the row was written
}

// Entry is a message to store, with optional sender information.
type Entry struct {
	Message    llm.Message
	SenderID   string
	SenderName string
	SentAt     string
}

// sanitizeHistory makes a stored history safe to send to a model: it must
// start with a user message, and every assistant tool-call message must be
// followed by results for all of its calls. Violations (left behind by a
// crash or a cut compaction) are dropped rather than sent, since providers
// reject them.
func sanitizeHistory(in []Stored) []Stored {
	out := make([]Stored, 0, len(in))
	i := 0
	for i < len(in) && in[i].Message.Role != llm.RoleUser {
		i++
	}
	for i < len(in) {
		m := in[i].Message
		switch {
		case m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0:
			j := i + 1
			results := map[string]bool{}
			for j < len(in) && in[j].Message.Role == llm.RoleTool {
				results[in[j].Message.ToolCallID] = true
				j++
			}
			complete := true
			for _, tc := range m.ToolCalls {
				if !results[tc.ID] {
					complete = false
					break
				}
			}
			if complete {
				out = append(out, in[i:j]...)
			}
			i = j
		case m.Role == llm.RoleTool:
			// orphaned tool result
			i++
		default:
			out = append(out, in[i])
			i++
		}
	}
	return out
}
