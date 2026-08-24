package tools

// skill.go
//
// Durable session skill catalog and model-facing `skill` loader tools
// (upstream `CK/packages/skill/tool-skill` + `skill-filesystem`). A skill is a
// directory (or a flat Markdown file) carrying `SKILL.md` with YAML frontmatter
// declaring `name` and `description`, plus optional invocation policy
// (`disable-model-invocation`, `user-invocable`).
//
// Discovery resolves skill roots from the caller's working directory upward and
// the user skill homes, with lower ranks winning duplicate names (mirroring
// skill-filesystem ranks). This implementation ships a model-facing catalog
// (`skill_list`) and loader (`skill`).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skillNamePattern mirrors upstream SKILL_NAME: kebab-case lower alphanumeric.
var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// skillDiscovery ranks (lower wins) mirror skill-filesystem.
const (
	skillRankProjectDsh    = 100
	skillRankProjectAgents = 200
	skillRankCustom        = 300
	skillRankUserDsh       = 400
	skillRankUserAgents    = 500
)

// skillDefinition is one discovered, model-loadable skill.
type skillDefinition struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	WhenToUse      string `json:"whenToUse,omitempty"`
	Provider       string `json:"provider"`
	ModelInvocable bool   `json:"modelInvocable"`
	UserInvocable  bool   `json:"userInvocable"`
	Content        string `json:"content"`
	Root           string `json:"root"`
	rank           int
}

// RegisterSkillTools registers the model-facing skill catalog and loader
// (upstream tool-skill: `skill` load + a catalog listing).
func (r *ToolRegistry) RegisterSkillTools() {
	r.Register(ToolDefinition{
		Name:        "skill",
		Description: "Load the full instructions for an available skill. Call this with the exact skill name from the session skill catalog before acting on a task that names or clearly matches that skill.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": { "type": "string", "description": "The exact skill name from the available skills list." }
			},
			"required": ["name"]
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			var args struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return nil, err
			}
			skills, err := discoverSkills(ctx.Cwd)
			if err != nil {
				return nil, err
			}
			skill, ok := skills[args.Name]
			if !ok {
				return nil, fmt.Errorf("skill %q is unknown or no longer available", args.Name)
			}
			if !skill.ModelInvocable {
				return nil, fmt.Errorf("skill %q is not available for model invocation", args.Name)
			}
			return map[string]any{
				"name":     skill.Name,
				"provider": "local",
				"resourceBase": map[string]any{
					"kind": "directory",
					"path": skill.Root,
				},
				"content": skill.Content,
			}, nil
		},
	})

	r.Register(ToolDefinition{
		Name:        "skill_list",
		Description: "List the skills available in this session as a model-facing catalog with their routing descriptions.",
		ParametersJSON: json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
		Execute: func(ctx ToolExecutionContext, argsJSON string) (any, error) {
			skills, err := discoverSkills(ctx.Cwd)
			if err != nil {
				return nil, err
			}
			var entries []skillDefinition
			for _, s := range skills {
				if !s.ModelInvocable {
					continue
				}
				entries = append(entries, s)
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			lines := []string{"A skill is a reusable set of task-specific instructions. The following skills are available in this session:"}
			for _, e := range entries {
				lines = append(lines, fmt.Sprintf("- `%s`: %s", e.Name, e.Description))
			}
			if len(entries) == 0 {
				lines = append(lines, "No skills are currently available through the `skill` tool.")
			}
			lines = append(lines,
				"If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions. This catalog contains summaries only; do not infer or follow a skill's instructions until it has been loaded.",
			)
			return strings.Join(lines, "\n"), nil
		},
	})
}

// discoverSkills walks the caller's project and user skill roots and merges
// candidates by name, lower rank winning (upstream skill-filesystem rank order).
func discoverSkills(cwd string) (map[string]skillDefinition, error) {
	result := map[string]skillDefinition{}
	for root, rank := range skillRoots(cwd) {
		candidates, err := scanSkillRoot(root, rank)
		if err != nil {
			// A missing/restricted root is not fatal; keep walking.
			continue
		}
		for name, cand := range candidates {
			existing, ok := result[name]
			if ok && existing.rank <= cand.rank {
				continue
			}
			result[name] = cand
		}
	}
	return result, nil
}

// skillRoots returns candidate skill roots from the caller cwd and user home,
// each with its discovery rank (lower wins) mirroring skill-filesystem.
func skillRoots(cwd string) map[string]int {
	roots := map[string]int{
		filepath.Join(cwd, ".dsh", "skills"):    skillRankProjectDsh,
		filepath.Join(cwd, ".agents", "skills"): skillRankProjectAgents,
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		roots[filepath.Join(home, ".dsh", "skills")] = skillRankUserDsh
		roots[filepath.Join(home, ".agents", "skills")] = skillRankUserAgents
	}
	return roots
}

// scanSkillRoot reads one skill root for directory-bundle skills and flat
// Markdown files, returning candidates keyed by name.
func scanSkillRoot(root string, rank int) (map[string]skillDefinition, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := map[string]skillDefinition{}
	for _, entry := range entries {
		if entry.IsDir() {
			mdPath := filepath.Join(root, entry.Name(), "SKILL.md")
			def, ok := parseSkillMarkdown(mdPath, root, entry.Name(), rank)
			if ok {
				out[def.Name] = def
			}
			continue
		}
		if strings.EqualFold(entry.Name(), "SKILL.md") {
			def, ok := parseSkillMarkdown(filepath.Join(root, entry.Name()), root, "", rank)
			if ok {
				out[def.Name] = def
			}
		}
	}
	return out, nil
}

// parseSkillMarkdown reads a SKILL.md file, splits YAML frontmatter, and
// builds a skillDefinition. Returns ok=false for invalid or non-model-eligible
// skills (name or description absent, name malformed, model invocation
// disabled).
func parseSkillMarkdown(path, root, dirName string, rank int) (skillDefinition, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return skillDefinition{}, false
	}
	data, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return skillDefinition{}, false
	}
	name := frontmatterString(data, "name")
	desc := frontmatterString(data, "description")
	if name == "" {
		name = dirName
	}
	if name == "" || desc == "" {
		return skillDefinition{}, false
	}
	if !skillNamePattern.MatchString(name) {
		return skillDefinition{}, false
	}
	disableModel := frontmatterBool(data, "disable-model-invocation")
	userInvocable := frontmatterBoolDefaultTrue(data, "user-invocable")
	return skillDefinition{
		Name:           name,
		Description:    desc,
		WhenToUse:      firstNonEmpty(frontmatterString(data, "when_to_use"), frontmatterString(data, "whenToUse")),
		Provider:       "local",
		ModelInvocable: !disableModel,
		UserInvocable:  userInvocable,
		Content:        strings.TrimSpace(body),
		Root:           filepath.Dir(path),
		rank:           rank,
	}, true
}

// splitFrontmatter splits leading `---`-delimited YAML frontmatter from the
// Markdown body. Returns ok=false when there is no complete frontmatter block.
func splitFrontmatter(raw string) (map[string]string, string, bool) {
	data := map[string]string{}
	if !strings.HasPrefix(raw, "---") {
		return nil, raw, false
	}
	rest := raw[len("---"):]
	// consume optional same-line remainder / newline
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	endIdx := strings.Index(rest, "\n---")
	if endIdx < 0 {
		return nil, raw, false
	}
	header := rest[:endIdx]
	body := rest[endIdx+len("\n---"):]
	body = strings.TrimPrefix(body, "\r\n")
	body = strings.TrimPrefix(body, "\n")
	parseFrontmatterLines(header, data)
	return data, body, true
}

// parseFrontmatterLines parses simple `key: value` frontmatter, folding
// continuation lines (indented or deeper on the same key) onto the value.
func parseFrontmatterLines(header string, out map[string]string) {
	var currentKey string
	var currentVal strings.Builder
	for _, rawLine := range strings.Split(header, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			// Continuation of the previous key's value.
			if currentKey != "" {
				currentVal.WriteString(" " + strings.TrimSpace(trimmed))
			}
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "" {
			continue
		}
		if currentKey != "" {
			out[currentKey] = strings.TrimSpace(currentVal.String())
		}
		currentKey = key
		currentVal.Reset()
		currentVal.WriteString(val)
	}
	if currentKey != "" {
		out[currentKey] = strings.TrimSpace(currentVal.String())
	}
}

func frontmatterString(data map[string]string, key string) string {
	if v, ok := data[key]; ok {
		v = strings.Trim(v, `"'`)
		return v
	}
	return ""
}

func frontmatterBool(data map[string]string, key string) bool {
	v := frontmatterString(data, key)
	switch strings.ToLower(v) {
	case "true", "yes", "1", "on":
		return true
	default:
		return false
	}
}

func frontmatterBoolDefaultTrue(data map[string]string, key string) bool {
	v := frontmatterString(data, key)
	switch strings.ToLower(v) {
	case "false", "no", "0", "off":
		return false
	default:
		return true
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
