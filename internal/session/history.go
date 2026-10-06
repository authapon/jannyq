package session

import "github.com/authapon/jannyq/internal/llm"

// Stored is a persisted message with its row ID.
type Stored struct {
	ID      int64
	Message llm.Message
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
