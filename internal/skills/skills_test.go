package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/tools"
)

func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	skill := Parse("C:/skills/review.md", []byte("---\nname: Code Review\ndescription: Review a change\n---\n\n# Steps\nRead the diff.\n"))
	if skill.Name != "code-review" {
		t.Errorf("name = %q", skill.Name)
	}
	if skill.Description != "Review a change" {
		t.Errorf("description = %q", skill.Description)
	}
	if !strings.Contains(skill.Body, "# Steps") || strings.Contains(skill.Body, "---") {
		t.Errorf("body = %q", skill.Body)
	}
}

func TestParseFallsBackToFilenameAndFirstLine(t *testing.T) {
	skill := Parse("/x/release_notes.md", []byte("# Heading\n\nDraft the release notes.\n\nMore.\n"))
	if skill.Name != "release_notes" {
		t.Errorf("name = %q", skill.Name)
	}
	if skill.Description != "Draft the release notes." {
		t.Errorf("description = %q", skill.Description)
	}
}

func TestDiscoverProjectOverridesHome(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	projectDir := filepath.Join(project, ".codedeep", "skills")
	homeDir := filepath.Join(home, ".codedeep", "skills")
	writeSkill(t, projectDir, "review.md", "---\ndescription: project review\n---\nproject body\n")
	writeSkill(t, homeDir, "review.md", "---\ndescription: home review\n---\nhome body\n")
	writeSkill(t, homeDir, "triage.md", "---\ndescription: triage issues\n---\ntriage body\n")

	c := Discover(project, home)
	if c.Len() != 2 {
		t.Fatalf("skills = %d, want 2", c.Len())
	}
	review, ok := c.Get("review")
	if !ok || review.Description != "project review" || review.Body != "project body" {
		t.Fatalf("project skill should win: %+v", review)
	}
	triage, ok := c.Get("TRIAGE")
	if !ok || triage.Description != "triage issues" {
		t.Fatalf("home skill missing: %+v", triage)
	}
}

func TestSectionAndPrompt(t *testing.T) {
	c := NewCatalog([]Skill{{Name: "review", Description: "review changes", Body: "Do it."}})
	section := c.Section()
	if !strings.Contains(section, "use_skill") || !strings.Contains(section, "review: review changes") {
		t.Fatalf("section = %q", section)
	}
	prompt := Skill{Name: "review", Body: "Do it."}.Prompt("check main.go")
	if !strings.HasPrefix(prompt, `Use the skill "review".`) || !strings.Contains(prompt, "# Task\ncheck main.go") {
		t.Fatalf("prompt = %q", prompt)
	}
	if NewCatalog(nil).Section() != "" {
		t.Fatal("empty catalog should render nothing")
	}
}

func TestUseSkillTool(t *testing.T) {
	c := NewCatalog([]Skill{{Name: "review", Body: "Read the diff."}})
	r := tools.NewRegistry()
	c.RegisterTools(r)
	spec, ok := r.Get("use_skill")
	if !ok {
		t.Fatal("use_skill was not registered")
	}
	res, err := spec.Run(context.Background(), json.RawMessage(`{"name":"review"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "Read the diff.") || res.IsError {
		t.Fatalf("output = %q", res.Output)
	}
	missing, err := spec.Run(context.Background(), json.RawMessage(`{"name":"nope"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !missing.IsError || !strings.Contains(missing.Output, "review") {
		t.Fatalf("missing skill response = %+v", missing)
	}
}

func TestEmptyCatalogRegistersNoTool(t *testing.T) {
	r := tools.NewRegistry()
	Discover(t.TempDir(), t.TempDir()).RegisterTools(r)
	if _, ok := r.Get("use_skill"); ok {
		t.Fatal("use_skill should not be registered without skills")
	}
}
