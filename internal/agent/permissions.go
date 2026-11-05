package agent

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strings"

	"codedeep/internal/approval"
	"codedeep/internal/config"
	"codedeep/internal/tools"
)

// evaluateRules returns the first definitive permission decision for a call.
// A deny match short-circuits; otherwise any allow match permits the call.
func (a *Agent) evaluateRules(tool string, raw json.RawMessage) (approval.Decision, bool) {
	if a.cfg == nil || len(a.cfg.Permissions) == 0 {
		return approval.Deny, false
	}
	command := argString(raw, "command")
	target := argString(raw, "path")
	allowed := false
	for _, rule := range a.cfg.Permissions {
		if !a.ruleMatches(rule, tool, command, target) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(rule.Action)) {
		case "deny":
			return approval.Deny, true
		case "allow":
			allowed = true
		}
	}
	if allowed {
		return approval.AllowOnce, true
	}
	return approval.Deny, false
}

func (a *Agent) ruleMatches(rule config.PermissionRule, tool, command, target string) bool {
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
	if rule.Path != "" {
		if target == "" || !matchPathRule(a.cfg.WorkDir, rule.Path, target) {
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
	}
	return name
}

func argString(raw json.RawMessage, key string) string {
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}
