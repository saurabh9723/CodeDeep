package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codedeep/internal/config"
	"codedeep/internal/index"
	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/skills"
)

func TestSystemPromptIncludesRankedRepoMap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "widget.go"), []byte("package widget\n\nfunc GadgetWidget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		WorkDir: dir,
		Home:    t.TempDir(),
		RepoMap: config.RepoMap{PromptTokens: 600, MaxFiles: 100, RefreshSec: 1},
	}
	ag := &Agent{
		cfg:    cfg,
		skills: skills.Discover(dir, cfg.Home),
		idx:    index.New(dir, index.Options{RefreshInterval: time.Nanosecond}),
	}
	msgs := []llm.Message{{Role: "user", Content: "please fix GadgetWidget"}}
	system := ag.systemPrompt(nil, msgs)
	if !strings.Contains(system, "# Codebase map") {
		t.Fatalf("prompt missing map section:\n%s", system)
	}
	if !strings.Contains(system, "GadgetWidget") {
		t.Fatalf("prompt missing relevant symbol:\n%s", system)
	}

	cfg.RepoMap.PromptTokens = 0
	system = ag.systemPrompt(nil, msgs)
	if strings.Contains(system, "# Codebase map") {
		t.Fatalf("map should be disabled at zero budget:\n%s", system)
	}
}

func TestLatestUserQuery(t *testing.T) {
	sess := &session.Session{Messages: []llm.Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "second"},
	}}
	if got := latestUserQuery(nil, sess); got != "second" {
		t.Fatalf("session query = %q", got)
	}
	msgs := []llm.Message{{Role: "user", Content: "subagent task"}}
	if got := latestUserQuery(msgs, sess); got != "subagent task" {
		t.Fatalf("live query = %q", got)
	}
	if got := latestUserQuery(nil, nil); got != "" {
		t.Fatalf("empty query = %q", got)
	}
}
