package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/authapon/jannyq/internal/skill"
)

// LoadSkill returns the instructions of a skill listed in the system prompt.
type LoadSkill struct {
	Store *skill.Store
}

func (l *LoadSkill) Name() string { return "load_skill" }

func (l *LoadSkill) Description() string {
	return "Load the full instructions of one of the available skills (listed in the system prompt). " +
		"Call this before doing a task that matches a skill's description, then follow the instructions. " +
		"With the file argument it reads another file from that skill's folder."
}

func (l *LoadSkill) Parameters() []byte {
	return []byte(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Skill name, exactly as listed."},
    "file": {"type": "string", "description": "Optional path of another file inside the skill folder, relative to it."}
  },
  "required": ["name"]
}`)
}

func (l *LoadSkill) Execute(_ context.Context, _ CallContext, raw []byte) (string, error) {
	var args struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	args.Name = strings.TrimSpace(args.Name)
	if args.Name == "" {
		return "", errors.New("name is required")
	}
	out, err := l.Store.Load(args.Name, strings.TrimSpace(args.File))
	if err != nil {
		if errors.Is(err, skill.ErrNotFound) {
			names := make([]string, 0)
			for _, s := range l.Store.Summaries() {
				names = append(names, s.Name)
			}
			return "", fmt.Errorf("%w (available skills: %s)", err, strings.Join(names, ", "))
		}
		return "", err
	}
	return out, nil
}
