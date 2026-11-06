package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/tools"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), t.TempDir())
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Build Command": "build-command",
		"  API/Notes  ": "api-notes",
		"a_b.c":         "a_b.c",
		"??":            "",
		"---":           "",
	}
	for in, want := range cases {
		got, err := NormalizeName(in)
		if want == "" {
			if err == nil {
				t.Errorf("NormalizeName(%q) = %q, want error", in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeName(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveGetListAndFormat(t *testing.T) {
	s := newTestStore(t)
	note, err := s.Save("project", "Build Command", "How to build", "# Build\n\nRun make build.")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if note.Name != "build-command" || note.Scope != "project" {
		t.Fatalf("note = %+v", note)
	}
	path := filepath.Join(s.ProjectDir, "build-command.md")
	if note.Path != path {
		t.Errorf("path = %q, want %q", note.Path, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "description: How to build") || !strings.Contains(string(data), "Run make build.") {
		t.Errorf("stored file = %q", data)
	}

	got, ok := s.Get("project", "build-command")
	if !ok || got.Description != "How to build" || !strings.Contains(got.Body, "make build") {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	if _, ok := s.Get("global", "build-command"); ok {
		t.Error("global lookup found a project note")
	}
	list := s.List()
	if len(list) != 1 || list[0].Name != "build-command" {
		t.Fatalf("List = %+v", list)
	}
	if section := s.PromptSection(8 << 10); !strings.Contains(section, "build-command [project]") {
		t.Errorf("PromptSection = %q", section)
	}
}

func TestDescriptionDefaultsToFirstLine(t *testing.T) {
	s := newTestStore(t)
	note, err := s.Save("project", "notes", "", "# Heading\n\nDetails here.")
	if err != nil {
		t.Fatal(err)
	}
	if note.Description != "Heading" {
		t.Errorf("description = %q, want Heading", note.Description)
	}
}

func TestProjectShadowsGlobal(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save("global", "shared", "global copy", "global body"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("project", "shared", "project copy", "project body"); err != nil {
		t.Fatal(err)
	}
	note, ok := s.Find("shared")
	if !ok {
		t.Fatal("Find returned no note")
	}
	if note.Scope != "project" || note.Description != "project copy" {
		t.Errorf("Find = %+v, want the project copy", note)
	}
	if list := s.List(); len(list) != 1 {
		t.Errorf("List length = %d, want 1 (shadowed global)", len(list))
	}
}

func TestSearchAndLimit(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save("project", "testing", "Run the test suite", "go test ./... in the repository root"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("project", "deploy", "Release process", "Tag and push to the registry"); err != nil {
		t.Fatal(err)
	}
	hits := s.Search("go test")
	if len(hits) != 1 || hits[0].Name != "testing" {
		t.Fatalf("Search(go test) = %+v", hits)
	}
	if hits := s.Search("registry"); len(hits) != 1 || hits[0].Name != "deploy" {
		t.Fatalf("Search(registry) = %+v", hits)
	}
	if hits := s.Search(""); len(hits) != 2 {
		t.Fatalf("Search(empty) returned %d notes, want 2", len(hits))
	}
	if hits := s.Search("nothing-matches-this"); len(hits) != 0 {
		t.Errorf("Search(unmatched) = %+v", hits)
	}

	for i := 0; i < maxResults+4; i++ {
		name := "note-" + strings.Repeat("x", i%3) + string(rune('a'+i))
		if _, err := s.Save("project", name, "common word", "common body"); err != nil {
			t.Fatal(err)
		}
	}
	if hits := s.Search("common"); len(hits) > maxResults {
		t.Errorf("Search returned %d hits, cap is %d", len(hits), maxResults)
	}
}

func TestSaveValidationAndDelete(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Save("project", "empty", "d", "   "); err == nil {
		t.Error("Save accepted an empty body")
	}
	if _, err := s.Save("galaxy", "name", "d", "body"); err == nil {
		t.Error("Save accepted an unknown scope")
	}
	if _, err := s.Delete("project", "missing"); err == nil {
		t.Error("Delete of a missing note succeeded")
	}
	if _, err := s.Save("global", "gone", "temp", "temporary body"); err != nil {
		t.Fatal(err)
	}
	note, err := s.Delete("global", "gone")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if note.Scope != "global" {
		t.Errorf("deleted note scope = %q", note.Scope)
	}
	if _, err := os.Stat(filepath.Join(s.GlobalDir, "gone.md")); !os.IsNotExist(err) {
		t.Errorf("note file still exists: %v", err)
	}
}

func TestPromptSectionIsBounded(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 30; i++ {
		name := "note-" + string(rune('a'+i))
		if _, err := s.Save("project", name, strings.Repeat("describe ", 8), "body"); err != nil {
			t.Fatal(err)
		}
	}
	section := s.PromptSection(300)
	if !strings.Contains(section, "more memory notes omitted") {
		t.Errorf("PromptSection was not truncated: %q", section)
	}
	if len(section) > 600 {
		t.Errorf("PromptSection length = %d, want a bounded section", len(section))
	}
}

func TestMemoryTools(t *testing.T) {
	s := newTestStore(t)
	reg := tools.NewRegistry()
	RegisterTools(reg, s)

	remember, ok := reg.Get("remember")
	if !ok {
		t.Fatal("remember not registered")
	}
	if !remember.Internal {
		t.Error("remember should be internal (no approval)")
	}
	out, err := remember.Run(context.Background(), json.RawMessage(`{"name":"Build Command","description":"how to build","body":"run make build"}`))
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if !strings.Contains(out.Output, "Saved memory") {
		t.Errorf("remember output = %q", out.Output)
	}

	recall, _ := reg.Get("recall")
	if !recall.ReadOnly {
		t.Error("recall should be read-only")
	}
	out, err = recall.Run(context.Background(), json.RawMessage(`{"query":"make build"}`))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(out.Output, "run make build") {
		t.Errorf("recall output = %q", out.Output)
	}
	out, err = recall.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("recall index: %v", err)
	}
	if !strings.Contains(out.Output, "build-command [project]") {
		t.Errorf("recall index = %q", out.Output)
	}
	out, err = recall.Run(context.Background(), json.RawMessage(`{"query":"absent-term"}`))
	if err != nil {
		t.Fatalf("recall miss: %v", err)
	}
	if !strings.Contains(out.Output, "No memory matched") {
		t.Errorf("recall miss = %q", out.Output)
	}

	forget, _ := reg.Get("forget")
	if !forget.Internal {
		t.Error("forget should be internal (no approval)")
	}
	out, err = forget.Run(context.Background(), json.RawMessage(`{"name":"build-command"}`))
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !strings.Contains(out.Output, "Deleted memory") {
		t.Errorf("forget output = %q", out.Output)
	}
	if _, ok := s.Find("build-command"); ok {
		t.Error("note survived forget")
	}
	if _, err := forget.Run(context.Background(), json.RawMessage(`{"name":"build-command"}`)); err == nil {
		t.Error("forget of a missing note succeeded")
	}
}
