package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Price is USD per million tokens used for harness cost accounting.
type Price struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheHitPerMTok   float64
	CacheWritePerMTok float64
}

func (p Price) configured() bool {
	return p.InputPerMTok != 0 || p.OutputPerMTok != 0 || p.CacheHitPerMTok != 0 || p.CacheWritePerMTok != 0
}

func (p Price) asMap() map[string]any {
	out := map[string]any{
		"input_per_mtok":  p.InputPerMTok,
		"output_per_mtok": p.OutputPerMTok,
	}
	if p.CacheHitPerMTok > 0 {
		out["cache_hit_per_mtok"] = p.CacheHitPerMTok
	}
	if p.CacheWritePerMTok > 0 {
		out["cache_write_per_mtok"] = p.CacheWritePerMTok
	}
	return out
}

// ProviderOptions describes where the evaluated agent gets model responses.
type ProviderOptions struct {
	Mode     Mode
	Provider string
	Model    string
	BaseURL  string
	Prices   Price
}

// mockPrice is deliberately small but exercises cache-hit and cache-write
// pricing in every mock run.
var mockPrice = Price{
	InputPerMTok:      1.0,
	OutputPerMTok:     2.0,
	CacheHitPerMTok:   0.1,
	CacheWritePerMTok: 1.25,
}

const mockModel = "mock-model"

// buildConfig produces the isolated config file for one task run. Connection
// fields are owned by the harness so a task cannot accidentally call a real
// provider or escape its workspace.
func buildConfig(task Task, opts ProviderOptions, mockURL, sessionsDir string) ([]byte, error) {
	base := map[string]any{
		"provider":      "openai",
		"model":         mockModel,
		"base_url":      mockURL,
		"approval":      "auto",
		"max_steps":     24,
		"prompt_cache":  true,
		"sessions_dir":  sessionsDir,
		"allow_outside": false,
		"mcp_servers":   map[string]any{},
		"pricing": map[string]any{
			mockModel: mockPrice.asMap(),
		},
	}
	if opts.Mode == ModeReal {
		base["provider"] = nonEmpty(opts.Provider, "openai")
		delete(base, "model")
		delete(base, "base_url")
		delete(base, "pricing")
		if opts.Model != "" {
			base["model"] = opts.Model
		}
		if opts.BaseURL != "" {
			base["base_url"] = opts.BaseURL
		}
		if opts.Prices.configured() {
			if opts.Model == "" {
				return nil, fmt.Errorf("price flags require -model in real mode")
			}
			base["pricing"] = map[string]any{opts.Model: opts.Prices.asMap()}
		}
	}
	merged, err := mergeMaps(base, task.Config)
	if err != nil {
		return nil, err
	}
	// Isolation and connection settings are not task-controlled.
	merged["sessions_dir"] = sessionsDir
	merged["allow_outside"] = false
	delete(merged, "api_key")
	if opts.Mode == ModeMock {
		merged["provider"] = "openai"
		merged["model"] = mockModel
		merged["base_url"] = mockURL
		pricing, _ := merged["pricing"].(map[string]any)
		if pricing == nil {
			pricing = map[string]any{}
			merged["pricing"] = pricing
		}
		if _, ok := pricing[mockModel]; !ok {
			pricing[mockModel] = mockPrice.asMap()
		}
	} else {
		merged["provider"] = nonEmpty(opts.Provider, "openai")
		if opts.Model == "" {
			delete(merged, "model")
		} else {
			merged["model"] = opts.Model
		}
		if opts.BaseURL == "" {
			delete(merged, "base_url")
		} else {
			merged["base_url"] = opts.BaseURL
		}
		if opts.Prices.configured() {
			pricing, _ := merged["pricing"].(map[string]any)
			if pricing == nil {
				pricing = map[string]any{}
				merged["pricing"] = pricing
			}
			pricing[opts.Model] = opts.Prices.asMap()
		}
	}
	return json.MarshalIndent(merged, "", "  ")
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

// mergeMaps recursively merges src over dst. Arrays and scalars replace.
func mergeMaps(dst, src map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for key, value := range src {
		if key == "" {
			return nil, fmt.Errorf("config contains an empty key")
		}
		if srcMap, ok := value.(map[string]any); ok {
			if dstMap, ok := out[key].(map[string]any); ok {
				merged, err := mergeMaps(dstMap, srcMap)
				if err != nil {
					return nil, err
				}
				out[key] = merged
				continue
			}
		}
		out[key] = value
	}
	return out, nil
}

// EnvOptions controls the isolated subprocess environment.
type EnvOptions struct {
	Mode        Mode
	Provider    string
	Model       string
	BaseURL     string
	Approval    string
	SessionsDir string
	Home        string
	GoCache     string
}

var realEnvKeep = map[string]bool{
	"CODEAGENT_API_KEY": true,
}

// buildEnv returns a deterministic environment with isolated HOME, sessions,
// and no inherited CodeDeep connection or MCP settings.
func buildEnv(opts EnvOptions) []string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	if opts.Mode == ModeMock {
		removePrefixes(env, "CODEAGENT_", "DEEPSEEK_", "OPENAI_", "ANTHROPIC_", "GEMINI_", "GOOGLE_", "OLLAMA_", "CLAUDE_")
		env["CODEAGENT_PROVIDER"] = nonEmpty(opts.Provider, "openai")
		env["CODEAGENT_MODEL"] = mockModel
		env["CODEAGENT_BASE_URL"] = opts.BaseURL
		env["DEEPSEEK_API_KEY"] = "eval-key"
	} else {
		removeKeys(env,
			"CODEAGENT_CONFIG", "CODEAGENT_MCP_CONFIG", "CODEAGENT_SESSIONS_DIR",
			"CODEAGENT_TRACES_DIR",
			"CODEAGENT_APPROVAL", "CODEAGENT_MAX_STEPS", "CODEAGENT_SHELL",
			"CODEAGENT_ALLOW_OUTSIDE", "CODEAGENT_DEBUG", "CODEAGENT_WEB_ADDR",
			"CODEAGENT_WEB_TOKEN", "CODEAGENT_WEB_INSECURE", "CODEAGENT_DIAGNOSTIC_TIMEOUT",
			"CODEAGENT_PROVIDER", "CODEAGENT_MODEL", "CODEAGENT_BASE_URL",
			"CODEAGENT_PROMPT_CACHE", "CODEAGENT_CONTEXT_WINDOW", "CODEAGENT_COMPACT_AT",
			"CODEAGENT_REASONING_EFFORT", "CODEAGENT_REASONING_BUDGET",
			"CODEAGENT_MAX_TOKENS", "CODEAGENT_TEMPERATURE",
			// Provider connection overrides must come from flags so a run is
			// reproducible even when the developer's shell exports these.
			"DEEPSEEK_MODEL", "DEEPSEEK_BASE_URL", "OPENAI_BASE_URL",
			"ANTHROPIC_BASE_URL", "GEMINI_BASE_URL", "GOOGLE_BASE_URL", "OLLAMA_BASE_URL",
		)
		for key := range env {
			if strings.HasPrefix(key, "CODEAGENT_") && !realEnvKeep[key] {
				delete(env, key)
			}
		}
		if opts.Provider != "" {
			env["CODEAGENT_PROVIDER"] = opts.Provider
		}
		if opts.Model != "" {
			env["CODEAGENT_MODEL"] = opts.Model
		}
		if opts.BaseURL != "" {
			env["CODEAGENT_BASE_URL"] = opts.BaseURL
		}
	}
	env["CODEAGENT_SESSIONS_DIR"] = opts.SessionsDir
	env["CODEAGENT_APPROVAL"] = nonEmpty(opts.Approval, "auto")
	env["CODEAGENT_CONFIG"] = ""
	env["CODEAGENT_MCP_CONFIG"] = ""
	env["NO_COLOR"] = "1"
	env["CLICOLOR"] = "0"
	env["TERM"] = "dumb"
	if opts.Home != "" {
		env["HOME"] = opts.Home
		env["USERPROFILE"] = opts.Home
	}
	if opts.GoCache != "" {
		if _, exists := env["GOCACHE"]; !exists {
			env["GOCACHE"] = opts.GoCache
		}
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func removePrefixes(env map[string]string, prefixes ...string) {
	for key := range env {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				delete(env, key)
				break
			}
		}
	}
}

func removeKeys(env map[string]string, keys ...string) {
	for _, key := range keys {
		delete(env, key)
	}
}

// CheckRealCredentials fails early with actionable guidance instead of letting
// every task fail with a provider 401.
func CheckRealCredentials(provider string) error {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ollama":
		return nil
	case "anthropic", "claude":
		if envAny("ANTHROPIC_API_KEY", "CODEAGENT_API_KEY") == "" {
			return fmt.Errorf("real Anthropic mode needs ANTHROPIC_API_KEY or CODEAGENT_API_KEY")
		}
	case "gemini", "google":
		if envAny("GEMINI_API_KEY", "GOOGLE_API_KEY", "CODEAGENT_API_KEY") == "" {
			return fmt.Errorf("real Gemini mode needs GEMINI_API_KEY, GOOGLE_API_KEY, or CODEAGENT_API_KEY")
		}
	default:
		if envAny("DEEPSEEK_API_KEY", "OPENAI_API_KEY", "CODEAGENT_API_KEY") == "" {
			return fmt.Errorf("real OpenAI-compatible mode needs DEEPSEEK_API_KEY, OPENAI_API_KEY, or CODEAGENT_API_KEY")
		}
	}
	return nil
}

func envAny(names ...string) string {
	for _, name := range names {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return name
		}
	}
	return ""
}

func writeConfig(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
