// Package skills discovers markdown instruction packs that extend the agent.
package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"codedeep/internal/tools"
)

const (
	maxSkillBytes       = 64 * 1024
	maxDescriptionChars = 200
)

// Skill is a named instruction document loaded from a markdown file.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Path        string `json:"path"`
}

// Catalog holds the skills visible to one workspace. Project skills override
// global skills with the same name.
type Catalog struct {
	list   []Skill
	byName map[string]Skill
}

func NewCatalog(list []Skill) *Catalog {
	c := &Catalog{byName: map[string]Skill{}}
	for _, s := range list {
		if _, exists := c.byName[s.Name]; exists {
			continue
		}
		c.byName[s.Name] = s
		c.list = append(c.list, s)
	}
	return c
}

func (c *Catalog) List() []Skill {
	if c == nil {
		return nil
	}
	return c.list
}

func (c *Catalog) Get(name string) (Skill, bool) {
	if c == nil {
		return Skill{}, false
	}
	s, ok := c.byName[normalizeName(name)]
	return s, ok
}

func (c *Catalog) Len() int {
	if c == nil {
		return 0
	}
	return len(c.list)
}

// Discover loads direct *.md children of <workDir>/.codedeep/skills and
// <home>/.codedeep/skills. Project files are read first and win on name
// conflicts.
func Discover(workDir, home string) *Catalog {
	var files []string
	seen := map[string]bool{}
	addDir := func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			p := filepath.Join(dir, entry.Name())
			if seen[p] {
				continue
			}
			seen[p] = true
			files = append(files, p)
		}
	}
	if workDir != "" {
		addDir(filepath.Join(workDir, ".codedeep", "skills"))
	}
	if home != "" {
		addDir(filepath.Join(home, ".codedeep", "skills"))
	}

	c := &Catalog{byName: map[string]Skill{}}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(data) > maxSkillBytes {
			data = data[:maxSkillBytes]
		}
		skill := Parse(path, data)
		if _, exists := c.byName[skill.Name]; exists {
			continue
		}
		c.byName[skill.Name] = skill
		c.list = append(c.list, skill)
	}
	return c
}

// Parse reads optional `---` frontmatter with name and description keys. A
// file without a name uses its filename; a missing description falls back to
// the first non-heading line of the body.
func Parse(path string, data []byte) Skill {
	name := normalizeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	description := ""
	body := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.HasPrefix(body, "---\n") {
		if end := strings.Index(body[4:], "\n---"); end >= 0 {
			front := body[4 : 4+end]
			body = strings.TrimLeft(body[4+end+4:], "\n")
			for _, line := range strings.Split(front, "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "name":
					if v := normalizeName(value); v != "" {
						name = v
					}
				case "description":
					description = strings.TrimSpace(value)
				}
			}
		}
	}
	if name == "" {
		name = "skill"
	}
	if description == "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			description = line
			break
		}
	}
	if len(description) > maxDescriptionChars {
		description = strings.TrimSpace(description[:maxDescriptionChars]) + "…"
	}
	return Skill{
		Name:        name,
		Description: description,
		Body:        strings.TrimSpace(body),
		Path:        path,
	}
}

// Prompt returns the skill instructions followed by the user's task.
func (s Skill) Prompt(task string) string {
	var b strings.Builder
	b.WriteString("Use the skill \"")
	b.WriteString(s.Name)
	b.WriteString("\".\n\n")
	b.WriteString(s.Body)
	task = strings.TrimSpace(task)
	if task != "" {
		b.WriteString("\n\n# Task\n")
		b.WriteString(task)
	}
	return b.String()
}

// Section renders the catalog for the system prompt. An empty catalog returns
// an empty string.
func (c *Catalog) Section() string {
	list := c.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`
# Skills
The workspace defines reusable skills. When a task matches one, call the
use_skill tool to load its full instructions before acting.
`)
	for _, s := range list {
		b.WriteString("\n- ")
		b.WriteString(s.Name)
		if s.Description != "" {
			b.WriteString(": ")
			b.WriteString(s.Description)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// RegisterTools exposes use_skill to the model.
func (c *Catalog) RegisterTools(r *tools.Registry) {
	if c == nil || c.Len() == 0 {
		return
	}
	r.Register(&tools.Spec{
		Name: "use_skill",
		Description: "Load the full instructions for a workspace skill. Call this before " +
			"performing a task that matches one of the skills listed in the system prompt.",
		ReadOnly: true,
		Params: tools.Object(map[string]any{
			"name": tools.StrProp("Skill name, for example \"review\"."),
		}, "name"),
		Run: func(_ context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return tools.Result{}, err
			}
			skill, ok := c.Get(a.Name)
			if !ok {
				names := make([]string, 0, len(c.list))
				for _, s := range c.list {
					names = append(names, s.Name)
				}
				return tools.Result{Output: "Unknown skill. Available skills: " + strings.Join(names, ", "), IsError: true}, nil
			}
			return tools.Result{Output: skill.Prompt("")}, nil
		},
	})
}

func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == ' ' || r == '.' || r == '/':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
