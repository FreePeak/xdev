package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/FreePeak/xdev/internal/skills"
	"github.com/FreePeak/xdev/internal/tool"
)

// LearnTool is the model-facing lesson recorder (M12, research F3 CORE):
// it stores the lesson FIRST, then optionally mints or updates a managed
// skill from the same call. A skill-write failure never loses the lesson.
type LearnTool struct {
	Backend Store
	// SkillsDir is the managed-skills root for `skill` payloads.
	SkillsDir string
	// Cwd is the project directory project-relative native skills are
	// resolved against when reporting shadowing. Empty means the process
	// working directory — the same anchor skill:// already uses.
	Cwd string
}

const LearnToolName = "learn"

func (t *LearnTool) Name() string { return LearnToolName }

func (t *LearnTool) Description() string {
	return "record a durable lesson (and optionally a reusable managed skill) so future sessions start with it; use for project conventions, non-obvious fixes, and user preferences"
}

func (t *LearnTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "memory": {"type": "string", "description": "the lesson: what to remember, when it applies, and why"},
    "context": {"type": "string", "description": "optional: what prompted the lesson (task, file, error)"},
    "skill": {
      "type": "object",
      "description": "optional managed skill to create or update alongside the lesson",
      "properties": {
        "action": {"type": "string", "enum": ["create", "update"]},
        "name": {"type": "string", "description": "kebab-case: [a-z0-9][a-z0-9-]{0,63}; a create over an existing authored name still writes but is reported shadowed"},
        "description": {"type": "string", "description": "one line, when to use the skill"},
        "body": {"type": "string", "description": "the SKILL.md body (markdown, no frontmatter)"}
      },
      "required": ["action", "name", "description", "body"]
    }
  },
  "required": ["memory"]
}`)
}

func (t *LearnTool) Execute(_ context.Context, args json.RawMessage) (tool.Result, error) {
	var a struct {
		Memory  string `json:"memory"`
		Context string `json:"context"`
		Skill   *struct {
			Action      string `json:"action"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Body        string `json:"body"`
		} `json:"skill"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tool.Result{Text: "learn: malformed arguments: " + err.Error(), IsError: true}, nil
	}
	if strings.TrimSpace(a.Memory) == "" {
		return tool.Result{Text: "learn: memory is required", IsError: true}, nil
	}
	// The lesson is stored FIRST: a skill problem must not lose it.
	if err := t.Backend.SaveLesson(a.Memory, a.Context); err != nil {
		return tool.Result{Text: "learn: " + err.Error(), IsError: true}, nil
	}
	out := "lesson recorded"
	var details any
	if a.Skill != nil {
		path, err := writeManagedSkill(t.SkillsDir, *a.Skill)
		if err != nil {
			// Partial success is reported honestly, not as a failure: the
			// lesson landed.
			return tool.Result{Text: out + "; skill not written: " + err.Error(), IsError: false}, nil
		}
		out += "; skill written to " + path
		if notice, d := t.shadow(a.Skill.Name, path); d != nil {
			out += "; " + notice
			details = d
		}
	}
	return tool.Result{Text: out, Details: details}, nil
}

// shadow reports the first-wins collision a just-written managed skill has
// with an authored pack. The write is never refused: the authored name is
// kept, the collision is named, and discovery decides which pack a session
// loads. It returns ("", nil) when nothing else carries the name.
func (t *LearnTool) shadow(name, written string) (string, any) {
	conflicts := skills.Conflicts(t.cwd(), name)
	if len(conflicts) == 0 {
		return "", nil
	}
	// Conflicts are in precedence order; the first one is the decisive
	// pair with the managed root.
	c := conflicts[0]
	if c.BeatsManaged {
		return fmt.Sprintf("shadowed: true — %s is discovered instead of the new managed skill (both files kept)", c.Skill.Path),
			map[string]any{"shadowed": true, "skill": written, "by": c.Skill.Path}
	}
	return fmt.Sprintf("shadowed: true — the new managed skill is discovered instead of %s", c.Skill.Path),
		map[string]any{"shadowed": true, "skill": c.Skill.Path, "by": written}
}

// cwd anchors project-relative native skills; empty means the process
// working directory.
func (t *LearnTool) cwd() string {
	if t.Cwd != "" {
		return t.Cwd
	}
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "."
}

// writeManagedSkill creates or updates <root>/<name>/SKILL.md. create
// fails when the file exists and update fails when it does not — the
// caller's intent is explicit, and silently doing the other would hide a
// mis-specified name.
func writeManagedSkill(root string, s struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}) (string, error) {
	if root == "" {
		return "", fmt.Errorf("no managed-skills directory configured")
	}
	if !validSkillName(s.Name) {
		return "", fmt.Errorf("invalid skill name %q (want [a-z0-9][a-z0-9-]{0,63})", s.Name)
	}
	if strings.TrimSpace(s.Description) == "" {
		return "", fmt.Errorf("skill description is required")
	}
	dir := fmt.Sprintf("%s/%s", strings.TrimRight(root, "/"), s.Name)
	path := dir + "/SKILL.md"
	_, statErr := os.Stat(path)
	exists := statErr == nil
	switch s.Action {
	case "create":
		if exists {
			return "", fmt.Errorf("skill %q already exists (use action=update)", s.Name)
		}
	case "update":
		if !exists {
			return "", fmt.Errorf("skill %q does not exist (use action=create)", s.Name)
		}
	default:
		return "", fmt.Errorf("action must be create or update")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if len(s.Body) > 64*1024 {
		return "", fmt.Errorf("skill body exceeds the 64KB cap")
	}
	doc := "---\nname: " + s.Name + "\ndescription: " + collapseWS(s.Description) + "\n---\n\n" + strings.TrimSpace(s.Body) + "\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func validSkillName(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for i, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}
