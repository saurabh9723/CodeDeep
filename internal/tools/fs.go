package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codedeep/internal/diff"
	"codedeep/internal/util"
)

const (
	maxSnapshotBytes = 1 << 20
	maxPreviewBytes  = 2 << 20
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
	mode := os.FileMode(0o644)
	oldContent := ""
	track := true
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return Result{}, fmt.Errorf("write_file: %s is a directory", f.Rel(abs))
		}
		existed = true
		mode = info.Mode().Perm()
		track = false
		if info.Size() <= maxSnapshotBytes {
			data, err := os.ReadFile(abs)
			if err == nil && !isBinary(data) {
				oldContent = string(data)
				track = true
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("write_file: %w", err)
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return Result{}, fmt.Errorf("write_file: %w", err)
	}
	var change *Change
	if track && len(a.Content) <= maxSnapshotBytes && !isBinary([]byte(a.Content)) {
		change = &Change{
			Path:    f.Rel(abs),
			Existed: existed,
			Mode:    uint32(mode.Perm()),
			Old:     oldContent,
			New:     a.Content,
		}
	}
	action := "created"
	if existed {
		action = "updated"
	}
	return Result{Output: fmt.Sprintf("%s %s (%d bytes)", action, f.Rel(abs), len(a.Content)), Change: change}, nil
}

func (f *FS) previewWrite(ctx context.Context, raw json.RawMessage) (*Preview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var a writeArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	abs, err := f.Resolve(a.Path)
	if err != nil {
		return nil, err
	}
	rel := f.Rel(abs)
	oldText := ""
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("write_file: %s is a directory", rel)
		}
		if info.Size() > maxPreviewBytes {
			return &Preview{Path: rel, Diff: fmt.Sprintf("[%s is %s; diff omitted]", rel, util.HumanBytes(int(info.Size())))}, nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		if isBinary(data) {
			return &Preview{Path: rel, Diff: fmt.Sprintf("[binary file %s; content will be replaced]", rel)}, nil
		}
		oldText = string(data)
	}
	if oldText == a.Content {
		return nil, nil
	}
	return &Preview{Path: rel, Diff: diff.Unified(rel, oldText, a.Content, 3)}, nil
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

type editMatch struct {
	count   int
	line    int
	updated string
}

// replaceText applies an exact-substring replacement and falls back to CRLF
// matching when the model emits LF but the file uses CRLF. It returns the
// updated content, the number of matches, and the 1-based line of the first
// match.
func replaceText(content, oldText, newText string, replaceAll bool, rel string) (string, int, int, error) {
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
		return "", 0, 0, fmt.Errorf("old_string was not found in %s. Re-read the file and match the exact text including indentation", rel)
	}
	if count > 1 && !replaceAll {
		return "", 0, 0, fmt.Errorf("old_string appears %d times in %s. Add surrounding context or set replace_all=true", count, rel)
	}
	line := 1 + strings.Count(content[:strings.Index(content, oldText)], "\n")
	return strings.ReplaceAll(content, oldText, newText), count, line, nil
}

func matchEdit(content string, a editArgs, rel string) (editMatch, error) {
	updated, count, line, err := replaceText(content, a.OldString, a.NewString, a.ReplaceAll, rel)
	if err != nil {
		return editMatch{}, fmt.Errorf("edit_file: %w", err)
	}
	return editMatch{
		count:   count,
		line:    line,
		updated: updated,
	}, nil
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
	m, err := matchEdit(content, a, f.Rel(abs))
	if err != nil {
		return Result{}, err
	}
	replacements := m.count
	if !a.ReplaceAll {
		replacements = 1
	}
	if err := os.WriteFile(abs, []byte(m.updated), info.Mode().Perm()); err != nil {
		return Result{}, fmt.Errorf("edit_file: %w", err)
	}
	var change *Change
	if len(content) <= maxSnapshotBytes && len(m.updated) <= maxSnapshotBytes && !isBinary(contentBytes) {
		change = &Change{
			Path:    f.Rel(abs),
			Existed: true,
			Mode:    uint32(info.Mode().Perm()),
			Old:     content,
			New:     m.updated,
		}
	}
	return Result{
		Output: fmt.Sprintf("edited %s (%d replacement(s), first at line %d)", f.Rel(abs), replacements, m.line),
		Change: change,
	}, nil
}

func (f *FS) previewEdit(ctx context.Context, raw json.RawMessage) (*Preview, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var a editArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.OldString == "" || a.OldString == a.NewString {
		return nil, nil
	}
	abs, err := f.Resolve(a.Path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() || info.Size() > maxPreviewBytes {
		return nil, nil
	}
	contentBytes, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if isBinary(contentBytes) {
		return &Preview{Path: f.Rel(abs), Diff: fmt.Sprintf("[binary file %s; diff omitted]", f.Rel(abs))}, nil
	}
	m, err := matchEdit(string(contentBytes), a, f.Rel(abs))
	if err != nil {
		return nil, err
	}
	return &Preview{
		Path: f.Rel(abs),
		Diff: diff.Unified(f.Rel(abs), string(contentBytes), m.updated, 3),
	}, nil
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}
