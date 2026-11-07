package agent

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/tools"
)

// evaluateRules returns the first definitive permission decision for a call.
// A deny rule blocks when it matches any affected path; an allow rule permits
// the call only when it matches every affected path, so a multi-file tool
// cannot smuggle one protected file past a path-scoped grant.
func (a *Agent) evaluateRules(tool string, raw json.RawMessage) (approval.Decision, bool) {
	if a.cfg == nil || len(a.cfg.Permissions) == 0 {
		return approval.Deny, false
	}
	command := argString(raw, "command")
	targets := argPaths(raw)
	allowed := false
	for _, rule := range a.cfg.Permissions {
		switch strings.ToLower(strings.TrimSpace(rule.Action)) {
		case "deny":
			if a.ruleMatchesAny(rule, tool, command, targets) {
				return approval.Deny, true
			}
		case "allow":
			if a.ruleMatchesAll(rule, tool, command, targets) {
				allowed = true
			}
		}
	}
	if allowed {
		return approval.AllowOnce, true
	}
	return approval.Deny, false
}

// ruleMatchesAny reports whether a rule covers at least one affected path.
// Deny rules use this form so one protected file blocks the whole call.
func (a *Agent) ruleMatchesAny(rule config.PermissionRule, tool, command string, targets []string) bool {
	if !a.ruleBaseMatches(rule, tool, command) {
		return false
	}
	if rule.Path == "" {
		return true
	}
	for _, target := range targets {
		if matchPathRule(a.cfg.WorkDir, rule.Path, target) {
			return true
		}
	}
	return false
}

// ruleMatchesAll reports whether a rule covers every affected path. Allow
// rules use this form so a multi-file tool cannot touch a file the grant does
// not cover.
func (a *Agent) ruleMatchesAll(rule config.PermissionRule, tool, command string, targets []string) bool {
	if !a.ruleBaseMatches(rule, tool, command) {
		return false
	}
	if rule.Path == "" {
		return true
	}
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if !matchPathRule(a.cfg.WorkDir, rule.Path, target) {
			return false
		}
	}
	return true
}

func (a *Agent) ruleBaseMatches(rule config.PermissionRule, tool, command string) bool {
	if strings.TrimSpace(rule.Tool) == "" {
		return false
	}
	if !globMatch(strings.ToLower(rule.Tool), strings.ToLower(tool)) {
		return false
	}
	if rule.Command != "" {
		if command == "" || !globMatch(rule.Command, command) {
			return false
		}
	}
	return true
}

func matchPathRule(workDir, pattern, value string) bool {
	if workDir == "" {
		workDir = "."
	}
	pattern = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pattern)), "./")
	value = filepath.ToSlash(strings.TrimSpace(value))
	if filepath.IsAbs(value) || strings.HasPrefix(value, "/") {
		if rel, err := filepath.Rel(workDir, filepath.FromSlash(value)); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, "../") {
			value = filepath.ToSlash(rel)
		}
	}
	value = strings.TrimPrefix(value, "./")
	if tools.Match(pattern, value) {
		return true
	}
	return !strings.Contains(pattern, "/") && tools.Match(pattern, path.Base(value))
}

// globMatch supports * and ? over the whole string (including slashes).
func globMatch(pattern, value string) bool {
	p := []rune(pattern)
	v := []rune(value)
	memo := map[[2]int]bool{}
	seen := map[[2]int]bool{}
	var rec func(i, j int) bool
	rec = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		var ok bool
		switch {
		case i == len(p):
			ok = j == len(v)
		case p[i] == '*':
			for k := j; k <= len(v); k++ {
				if rec(i+1, k) {
					ok = true
					break
				}
			}
		case j < len(v) && (p[i] == '?' || p[i] == v[j]):
			ok = rec(i+1, j+1)
		}
		memo[key] = ok
		return ok
	}
	return rec(0, 0)
}

// allowKey narrows an "always allow" grant to the exact command or file when
// the tool supports it.
func allowKey(name, args string) string {
	raw := json.RawMessage(args)
	switch name {
	case "bash":
		if cmd := argString(raw, "command"); cmd != "" {
			return name + "\x00cmd\x00" + cmd
		}
	case "write_file", "edit_file":
		if p := argString(raw, "path"); p != "" {
			return name + "\x00path\x00" + p
		}
	case "apply_patch":
		if paths := argPaths(raw); len(paths) > 0 {
			sort.Strings(paths)
			return name + "\x00paths\x00" + strings.Join(paths, "\x00")
		}
	}
	return name
}

// argPaths collects every path a tool call can touch: the single "path"
// argument plus the paths of apply_patch edits.
func argPaths(raw json.RawMessage) []string {
	var args struct {
		Path  string `json:"path"`
		Edits []struct {
			Path string `json:"path"`
		} `json:"edits"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil
	}
	var out []string
	if strings.TrimSpace(args.Path) != "" {
		out = append(out, args.Path)
	}
	for _, edit := range args.Edits {
		if strings.TrimSpace(edit.Path) != "" {
			out = append(out, edit.Path)
		}
	}
	return out
}

func argString(raw json.RawMessage, key string) string {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}
