package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"codedeep/internal/util"
)

// RegisterGit adds read-only tools for inspecting the repository state.
func RegisterGit(r *Registry, f *FS) {
	r.Register(&Spec{
		Name: "git_status",
		Description: "Show the current git branch, upstream state, last commit, and uncommitted files. " +
			"Use it before reviewing or changing code, and after a session of edits.",
		ReadOnly: true,
		Params:   Object(map[string]any{}),
		Run:      f.gitStatus,
	})
	r.Register(&Spec{
		Name: "git_diff",
		Description: "Show a unified diff of uncommitted changes. Read-only. Use path to restrict the diff " +
			"to one file or directory, staged=true for the index, and stat=true for a summary. " +
			"Prefer this over running git diff through bash.",
		ReadOnly: true,
		Params: Object(map[string]any{
			"path":    StrProp("Optional file or directory to diff, relative to the workspace."),
			"staged":  BoolProp("Diff the staged index instead of the working tree."),
			"stat":    BoolProp("Return a diffstat summary instead of the full diff."),
			"context": IntProp("Lines of context around each change.", 3),
		}),
		Run: f.gitDiff,
	})
}

type gitDiffArgs struct {
	Path    string `json:"path"`
	Staged  bool   `json:"staged"`
	Stat    bool   `json:"stat"`
	Context int    `json:"context"`
}

func (f *FS) gitStatus(ctx context.Context, raw json.RawMessage) (Result, error) {
	if !f.isGitRepo(ctx) {
		return Result{Output: "This workspace is not a git repository."}, nil
	}
	var b strings.Builder
	if out, err := gitOutput(ctx, f.Root, "status", "--porcelain=v1", "--branch"); err == nil {
		b.WriteString(out)
		b.WriteString("\n")
	}
	if out, err := gitOutput(ctx, f.Root, "log", "-1", "--oneline"); err == nil && strings.TrimSpace(out) != "" {
		fmt.Fprintf(&b, "last commit: %s\n", out)
	}
	if out, err := gitOutput(ctx, f.Root, "diff", "--stat"); err == nil && strings.TrimSpace(out) != "" {
		b.WriteString("unstaged:\n")
		b.WriteString(out)
		b.WriteString("\n")
	}
	if out, err := gitOutput(ctx, f.Root, "diff", "--cached", "--stat"); err == nil && strings.TrimSpace(out) != "" {
		b.WriteString("staged:\n")
		b.WriteString(out)
		b.WriteString("\n")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "Working tree is clean."
	}
	return Result{Output: util.TruncateMiddle(out, 128*1024)}, nil
}

func (f *FS) gitDiff(ctx context.Context, raw json.RawMessage) (Result, error) {
	if !f.isGitRepo(ctx) {
		return Result{Output: "This workspace is not a git repository."}, nil
	}
	var a gitDiffArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if a.Staged {
		args = append(args, "--cached")
	}
	if a.Stat {
		args = append(args, "--stat")
	} else {
		n := a.Context
		if n < 0 {
			n = 3
		}
		if n > 50 {
			n = 50
		}
		args = append(args, fmt.Sprintf("-U%d", n))
	}
	if strings.TrimSpace(a.Path) != "" {
		abs, err := f.Resolve(a.Path)
		if err != nil {
			return Result{}, err
		}
		args = append(args, "--", f.Rel(abs))
	}
	out, err := gitOutput(ctx, f.Root, args...)
	if err != nil {
		return Result{}, fmt.Errorf("git_diff: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return Result{Output: "No changes."}, nil
	}
	return Result{Output: util.TruncateMiddle(out, 200*1024)}, nil
}

func (f *FS) isGitRepo(ctx context.Context) bool {
	_, err := gitOutput(ctx, f.Root, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// GitBrief returns a one-line repository summary for the system prompt, or an
// empty string outside a git repository.
func GitBrief(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := gitOutput(ctx, root, "status", "--porcelain=v1", "--branch")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(out, "\r\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "## ") {
		return ""
	}
	head := strings.TrimPrefix(lines[0], "## ")
	branch, extra := head, ""
	if idx := strings.Index(branch, "..."); idx >= 0 {
		branch = branch[:idx]
	}
	if idx := strings.Index(branch, " ["); idx >= 0 {
		extra = strings.TrimSuffix(branch[idx+1:], "]")
		branch = branch[:idx]
	}
	changed := 0
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			changed++
		}
	}
	var b strings.Builder
	b.WriteString(branch)
	if extra != "" {
		b.WriteString(" (" + extra + ")")
	}
	if changed == 0 {
		b.WriteString(" · clean")
	} else {
		fmt.Fprintf(&b, " · %d uncommitted file(s)", changed)
	}
	return b.String()
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			msg = "git timed out"
		}
		return stdout.String(), errors.New(msg)
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}
