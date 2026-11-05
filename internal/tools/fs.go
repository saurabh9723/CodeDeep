package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codedeep/internal/util"
)

// FS confines tool paths to a workspace root.
type FS struct {
	Root         string
	AllowOutside bool
}

func NewFS(root string, allowOutside bool) *FS {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	return &FS{Root: root, AllowOutside: allowOutside}
}

func (f *FS) Resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Root, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if !f.AllowOutside {
		if err := f.ensureInside(abs); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func (f *FS) ensureInside(abs string) error {
	inside := func(candidate string) bool {
		rel, err := filepath.Rel(f.Root, candidate)
		if err != nil {
			return false
		}
		return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !inside(abs) {
		return fmt.Errorf("path %q is outside the workspace %q", abs, f.Root)
	}
	// Guard against symlinks escaping the workspace.
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		if !inside(real) {
			return fmt.Errorf("path %q resolves outside the workspace", abs)
		}
		return nil
	}
	if real, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil && !inside(real) {
		return fmt.Errorf("path %q resolves outside the workspace", abs)
	}
	return nil
}

func (f *FS) Rel(abs string) string {
	if rel, err := filepath.Rel(f.Root, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return abs
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (f *FS) readFile(ctx context.Context, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var a readArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	abs, err := f.Resolve(a.Path)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read_file: %w", err)
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("read_file: %s is a directory (use glob to list files)", f.Rel(abs))
	}
	file, err := os.Open(abs)
	if err != nil {
		return Result{}, fmt.Errorf("read_file: %w", err)
	}
	defer file.Close()

	head := make([]byte, 8192)
	n, _ := file.Read(head)
	if strings.IndexByte(string(head[:n]), 0) >= 0 {
		return Result{}, fmt.Errorf("read_file: %s appears to be a binary file", f.Rel(abs))
	}
	if _, err := file.Seek(0, 0); err != nil {
		return Result{}, err
	}

	offset := a.Offset
	if offset < 1 {
		offset = 1
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 400
	}
	if limit > 2000 {
		limit = 2000
	}

	rel := f.Rel(abs)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var b strings.Builder
	lineNo := 0
	shown := 0
	truncated := false
	sawRequested := false
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if shown >= limit || b.Len() >= 256*1024 {
			truncated = true
			break
		}
		text := strings.TrimSuffix(scanner.Text(), "\r")
		if len(text) > 2000 {
			text = util.Truncate(text, 2000)
		}
		fmt.Fprintf(&b, "%6d→%s\n", lineNo, text)
		shown++
		sawRequested = true
	}
	if err := scanner.Err(); err != nil {
		return Result{}, fmt.Errorf("read_file: %w", err)
	}
	if lineNo == 0 {
		return Result{Output: "[file is empty]"}, nil
	}
	if !sawRequested {
		return Result{}, fmt.Errorf("read_file: offset %d is beyond the end of %s (%d lines)", offset, rel, lineNo)
	}
	if truncated {
		fmt.Fprintf(&b, "\n[showing %d lines from line %d; more lines available]\n", shown, offset)
	}
	return Result{Output: b.String()}, nil
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (f *FS) writeFile(ctx context.Context, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var a writeArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	abs, err := f.Resolve(a.Path)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(a.Path) == "" {
		return Result{}, errors.New("write_file: path is required")
	}
	existed := false
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return Result{}, fmt.Errorf("write_file: %s is a directory", f.Rel(abs))
		}
		existed = true
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("write_file: %w", err)
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write_file: %w", err)
	}
	action := "created"
	if existed {
		action = "updated"
	}
	return Result{Output: fmt.Sprintf("%s %s (%d bytes)", action, f.Rel(abs), len(a.Content))}, nil
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (f *FS) editFile(ctx context.Context, raw json.RawMessage) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	var a editArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	if a.OldString == "" {
		return Result{}, errors.New("edit_file: old_string is required and must not be empty")
	}
	if a.OldString == a.NewString {
		return Result{}, errors.New("edit_file: old_string and new_string are identical")
	}
	abs, err := f.Resolve(a.Path)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("edit_file: %w", err)
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("edit_file: %s is a directory", f.Rel(abs))
	}
	if info.Size() > 20*1024*1024 {
		return Result{}, fmt.Errorf("edit_file: %s is too large to edit", f.Rel(abs))
	}
	contentBytes, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("edit_file: %w", err)
	}
	content := string(contentBytes)

	oldText, newText := a.OldString, a.NewString
	count := strings.Count(content, oldText)
	if count == 0 && strings.Contains(content, "\r\n") {
		// The model usually emits LF; the file may use CRLF.
		toCRLF := func(s string) string {
			s = strings.ReplaceAll(s, "\r\n", "\n")
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		candidate := toCRLF(oldText)
		if c := strings.Count(content, candidate); c > 0 {
			oldText, newText, count = candidate, toCRLF(newText), c
		}
	}
	if count == 0 {
		return Result{}, fmt.Errorf("edit_file: old_string was not found in %s. Re-read the file and match the exact text including indentation", f.Rel(abs))
	}
	if count > 1 && !a.ReplaceAll {
		return Result{}, fmt.Errorf("edit_file: old_string appears %d times in %s. Add surrounding context or set replace_all=true", count, f.Rel(abs))
	}
	line := 1 + strings.Count(content[:strings.Index(content, oldText)], "\n")
	replacements := count
	if !a.ReplaceAll {
		replacements = 1
	}
	updated := strings.ReplaceAll(content, oldText, newText)
	if err := os.WriteFile(abs, []byte(updated), info.Mode().Perm()); err != nil {
		return Result{}, fmt.Errorf("edit_file: %w", err)
	}
	return Result{Output: fmt.Sprintf("edited %s (%d replacement(s), first at line %d)", f.Rel(abs), replacements, line)}, nil
}
