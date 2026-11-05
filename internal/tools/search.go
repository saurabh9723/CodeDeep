package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"codedeep/internal/util"
)

var errStopWalk = errors.New("stop walk")

var ignoredDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "bower_components": true, "vendor": true,
	"dist": true, "build": true, "target": true, "out": true,
	".next": true, ".nuxt": true, ".svelte-kit": true,
	".cache": true, "coverage": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	".venv": true, "venv": true, "env": true,
	".idea": true, ".gradle": true,
}

func shouldSkipDir(name string) bool { return ignoredDirs[name] }

// Match implements glob matching with ** support. Both pattern and name use
// forward slashes and are relative.
func Match(pattern, name string) bool {
	p := splitGlob(pattern)
	n := splitGlob(name)
	memo := map[[2]int]int8{}
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		key := [2]int{i, j}
		if v, ok := memo[key]; ok {
			return v == 2
		}
		var result bool
		switch {
		case i == len(p):
			result = j == len(n)
		case p[i] == "**":
			for k := j; k <= len(n); k++ {
				if rec(i+1, k) {
					result = true
					break
				}
			}
		case j < len(n):
			if ok, err := path.Match(p[i], n[j]); err == nil && ok {
				result = rec(i+1, j+1)
			}
		}
		if result {
			memo[key] = 2
		} else {
			memo[key] = 1
		}
		return result
	}
	return rec(0, 0)
}

func splitGlob(p string) []string {
	p = filepath.ToSlash(strings.TrimSpace(p))
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." {
		return nil
	}
	return strings.Split(p, "/")
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

func (f *FS) globFiles(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a globArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Result{}, errors.New("glob: pattern is required")
	}
	if filepath.IsAbs(a.Pattern) || strings.HasPrefix(a.Pattern, "/") {
		return Result{}, errors.New("glob: pattern must be relative to the workspace")
	}
	searchPath := a.Path
	if strings.TrimSpace(searchPath) == "" {
		searchPath = "."
	}
	root, err := f.Resolve(searchPath)
	if err != nil {
		return Result{}, err
	}
	max := a.MaxResults
	if max <= 0 {
		max = 200
	}
	if max > 1000 {
		max = 1000
	}

	info, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("glob: %w", err)
	}
	if !info.IsDir() {
		if Match(a.Pattern, filepath.Base(root)) || Match(a.Pattern, f.Rel(root)) {
			return Result{Output: f.Rel(root)}, nil
		}
		return Result{Output: fmt.Sprintf("No files matched %q", a.Pattern)}, nil
	}

	var results []string
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, entryErr error) error {
		if entryErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		if Match(a.Pattern, filepath.ToSlash(rel)) {
			results = append(results, f.Rel(p))
			if len(results) >= max {
				return errStopWalk
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
		return Result{}, fmt.Errorf("glob: %w", walkErr)
	}
	if len(results) == 0 {
		return Result{Output: fmt.Sprintf("No files matched %q under %s", a.Pattern, f.Rel(root))}, nil
	}
	sort.Strings(results)
	out := strings.Join(results, "\n")
	if len(results) >= max {
		out += fmt.Sprintf("\n[stopped after %d results; narrow the pattern or raise max_results]", max)
	}
	return Result{Output: out}, nil
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	MaxResults int    `json:"max_results"`
}

func (f *FS) grepFiles(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a grepArgs
	if err := decodeArgs(raw, &a); err != nil {
		return Result{}, err
	}
	if a.Pattern == "" {
		return Result{}, errors.New("grep: pattern is required")
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Result{}, fmt.Errorf("grep: invalid regular expression: %w", err)
	}
	searchPath := a.Path
	if strings.TrimSpace(searchPath) == "" {
		searchPath = "."
	}
	root, err := f.Resolve(searchPath)
	if err != nil {
		return Result{}, err
	}
	max := a.MaxResults
	if max <= 0 {
		max = 100
	}
	if max > 1000 {
		max = 1000
	}
	info, err := os.Stat(root)
	if err != nil {
		return Result{}, fmt.Errorf("grep: %w", err)
	}

	var results []string
	process := func(abs string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		relFromRoot, _ := filepath.Rel(root, abs)
		if !matchGlobFilter(a.Glob, filepath.ToSlash(relFromRoot)) {
			return nil
		}
		fi, err := os.Stat(abs)
		if err != nil || fi.Size() > 5*1024*1024 {
			return nil
		}
		if isBinaryFile(abs) {
			return nil
		}
		file, err := os.Open(abs)
		if err != nil {
			return nil
		}
		defer file.Close()
		reader := bufio.NewReaderSize(file, 64*1024)
		lineNo := 0
		display := f.Rel(abs)
		for {
			line, readErr := reader.ReadString('\n')
			if line != "" {
				lineNo++
				text := strings.TrimRight(line, "\r\n")
				if re.MatchString(text) {
					results = append(results, fmt.Sprintf("%s:%d: %s", display, lineNo, util.Truncate(text, 500)))
					if len(results) >= max {
						return errStopWalk
					}
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					break
				}
				return nil
			}
		}
		return nil
	}

	if info.IsDir() {
		walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, entryErr error) error {
			if entryErr != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				if p != root && shouldSkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			return process(p)
		})
		if walkErr != nil && !errors.Is(walkErr, errStopWalk) {
			return Result{}, fmt.Errorf("grep: %w", walkErr)
		}
	} else {
		if err := process(root); err != nil && !errors.Is(err, errStopWalk) {
			return Result{}, fmt.Errorf("grep: %w", err)
		}
	}

	if len(results) == 0 {
		return Result{Output: fmt.Sprintf("No matches for `%s`.", a.Pattern)}, nil
	}
	out := strings.Join(results, "\n")
	if len(results) >= max {
		out += fmt.Sprintf("\n[stopped after %d matches; refine the pattern or raise max_results]", max)
	}
	return Result{Output: out}, nil
}

func matchGlobFilter(glob, rel string) bool {
	if strings.TrimSpace(glob) == "" {
		return true
	}
	if strings.Contains(glob, "/") {
		return Match(glob, rel)
	}
	return Match(glob, filepath.Base(rel))
}

func isBinaryFile(abs string) bool {
	file, err := os.Open(abs)
	if err != nil {
		return true
	}
	defer file.Close()
	head := make([]byte, 8192)
	n, _ := file.Read(head)
	return bytes.IndexByte(head[:n], 0) >= 0
}
