package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"codedeep/internal/llm"
)

// Price is an optional per-model price in USD per million tokens. When it is
// configured the agent reports estimated costs alongside token counts.
type Price struct {
	InputPerMTok    float64 `json:"input_per_mtok"`
	OutputPerMTok   float64 `json:"output_per_mtok"`
	CacheHitPerMTok float64 `json:"cache_hit_per_mtok,omitempty"`
}

type MCPServer struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Disabled   bool              `json:"disabled,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
}

// PermissionRule grants or denies a tool call without prompting. Command and
// Path are optional globs; when both are set the rule requires both to match.
// A deny rule always wins over an allow rule and over auto-approval.
type PermissionRule struct {
	Tool    string `json:"tool"`
	Action  string `json:"action"`
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
}

// Hook runs a shell command around a tool call. The hook receives a JSON
// payload on stdin. Pre-tool hooks can block execution by exiting non-zero
// when BlockOnFailure is set. Tools limits a hook to matching tool names.
type Hook struct {
	Command        string   `json:"command"`
	TimeoutSec     int      `json:"timeout_sec,omitempty"`
	Tools          []string `json:"tools,omitempty"`
	BlockOnFailure bool     `json:"block_on_failure,omitempty"`
}

type Hooks struct {
	PreTool  []Hook `json:"pre_tool,omitempty"`
	PostTool []Hook `json:"post_tool,omitempty"`
}

// DiagnosticRule runs a command after a matching file is changed. The command
// may use {file} (workspace-relative) and {abs} placeholders.
type DiagnosticRule struct {
	Pattern string `json:"pattern"`
	Command string `json:"command"`
}

// Editor describes how to open a file in an external editor. File and Line
// are replaced with the target path and line number in Args.
type Editor struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type Config struct {
	Provider          string               `json:"provider"`
	APIKey            string               `json:"api_key"`
	BaseURL           string               `json:"base_url"`
	Model             string               `json:"model"`
	MaxTokens         int                  `json:"max_tokens"`
	Temperature       float64              `json:"temperature"`
	ContextWindow     int                  `json:"context_window"`
	CompactAt         float64              `json:"compact_at"`
	Approval          string               `json:"approval"`
	WorkDir           string               `json:"-"`
	SessionsDir       string               `json:"sessions_dir"`
	Shell             string               `json:"shell"`
	MaxSteps          int                  `json:"max_steps"`
	AllowOutside      bool                 `json:"allow_outside"`
	WebAddr           string               `json:"web_addr"`
	Debug             bool                 `json:"debug"`
	MCPServers        map[string]MCPServer `json:"mcp_servers"`
	InstructionFiles  []string             `json:"instruction_files"`
	Pricing           map[string]Price     `json:"pricing,omitempty"`
	Permissions       []PermissionRule     `json:"permissions,omitempty"`
	Hooks             Hooks                `json:"hooks,omitempty"`
	Diagnostics       []DiagnosticRule     `json:"diagnostics,omitempty"`
	DiagnosticTimeout int                  `json:"diagnostic_timeout_sec,omitempty"`
	WebToken          string               `json:"web_token,omitempty"`
	WebInsecure       bool                 `json:"web_insecure,omitempty"`
	Editor            *Editor              `json:"editor,omitempty"`
	ConfigPath        string               `json:"-"`
	Home              string               `json:"-"`

	approvalMu sync.RWMutex
}

// ApprovalMode returns the current approval mode. The mode can be changed at
// runtime from the web UI, so readers must use this instead of the field.
func (c *Config) ApprovalMode() string {
	c.approvalMu.RLock()
	defer c.approvalMu.RUnlock()
	return c.Approval
}

// SetApproval changes the approval mode at runtime.
func (c *Config) SetApproval(mode string) {
	c.approvalMu.Lock()
	defer c.approvalMu.Unlock()
	c.Approval = mode
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	wd, _ := os.Getwd()
	return &Config{
		Provider:      "openai",
		BaseURL:       "https://api.deepseek.com",
		Model:         "deepseek-chat",
		MaxTokens:     8192,
		Temperature:   0.3,
		ContextWindow: 128000,
		CompactAt:     0.8,
		Approval:      "ask",
		WorkDir:       wd,
		SessionsDir:   filepath.Join(home, ".codedeep", "sessions"),
		MaxSteps:      60,
		WebAddr:       "127.0.0.1:7777",
		MCPServers:    map[string]MCPServer{},
		InstructionFiles: []string{
			"AGENTS.md",
			"CODEAGENT.md",
			filepath.Join(".codedeep", "AGENTS.md"),
		},
		Home: home,
	}
}

func DefaultConfigPath() string {
	if p := os.Getenv("CODEAGENT_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codedeep", "config.json")
}

// Load reads the config file (if present), then applies environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	explicit := path != ""
	if path == "" {
		path = DefaultConfigPath()
	}
	cfg.ConfigPath = path

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
		// Accept Claude-style "mcpServers" too.
		var alt struct {
			MCPServers map[string]MCPServer `json:"mcpServers"`
		}
		if json.Unmarshal(data, &alt) == nil && len(alt.MCPServers) > 0 {
			for name, srv := range alt.MCPServers {
				if _, exists := cfg.MCPServers[name]; !exists {
					cfg.MCPServers[name] = srv
				}
			}
		}
	case os.IsNotExist(err) && !explicit:
		// first run: defaults are fine
	default:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := cfg.applyMCPConfigFile(); err != nil {
		return nil, err
	}
	cfg.applyEnv()
	cfg.normalize()
	return cfg, nil
}

func (c *Config) applyMCPConfigFile() error {
	path := os.Getenv("CODEAGENT_MCP_CONFIG")
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read MCP config %s: %w", path, err)
	}
	var body struct {
		MCPServers map[string]MCPServer `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("parse MCP config %s: %w", path, err)
	}
	if c.MCPServers == nil {
		c.MCPServers = map[string]MCPServer{}
	}
	for name, srv := range body.MCPServers {
		c.MCPServers[name] = srv
	}
	return nil
}

func envFirst(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func (c *Config) applyEnv() {
	if v := envFirst("CODEAGENT_PROVIDER"); v != "" {
		c.Provider = v
	}
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "anthropic", "claude":
		if v := envFirst("ANTHROPIC_API_KEY", "CODEAGENT_API_KEY"); v != "" {
			c.APIKey = v
		}
		if v := envFirst("ANTHROPIC_BASE_URL", "CODEAGENT_BASE_URL"); v != "" {
			c.BaseURL = v
		}
	case "gemini", "google":
		if v := envFirst("GEMINI_API_KEY", "GOOGLE_API_KEY", "CODEAGENT_API_KEY"); v != "" {
			c.APIKey = v
		}
		if v := envFirst("GEMINI_BASE_URL", "CODEAGENT_BASE_URL"); v != "" {
			c.BaseURL = v
		}
	default:
		if v := envFirst("DEEPSEEK_API_KEY", "CODEAGENT_API_KEY", "OPENAI_API_KEY"); v != "" {
			c.APIKey = v
		}
		if v := envFirst("DEEPSEEK_BASE_URL", "CODEAGENT_BASE_URL", "OPENAI_BASE_URL"); v != "" {
			c.BaseURL = v
		}
	}
	if v := envFirst("CODEAGENT_MODEL", "DEEPSEEK_MODEL"); v != "" {
		c.Model = v
	}
	if v := os.Getenv("CODEAGENT_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxTokens = n
		}
	}
	if v := os.Getenv("CODEAGENT_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.Temperature = f
		}
	}
	if v := os.Getenv("CODEAGENT_CONTEXT_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 1000 {
			c.ContextWindow = n
		}
	}
	if v := os.Getenv("CODEAGENT_COMPACT_AT"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
			c.CompactAt = f
		}
	}
	if v := os.Getenv("CODEAGENT_APPROVAL"); v != "" {
		c.Approval = v
	}
	if v := os.Getenv("CODEAGENT_SESSIONS_DIR"); v != "" {
		c.SessionsDir = v
	}
	if v := os.Getenv("CODEAGENT_SHELL"); v != "" {
		c.Shell = v
	}
	if v := os.Getenv("CODEAGENT_MAX_STEPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MaxSteps = n
		}
	}
	if v := os.Getenv("CODEAGENT_WEB_ADDR"); v != "" {
		c.WebAddr = v
	}
	if v := os.Getenv("CODEAGENT_DEBUG"); v != "" {
		c.Debug = parseBool(v)
	}
	if v := os.Getenv("CODEAGENT_ALLOW_OUTSIDE"); v != "" {
		c.AllowOutside = parseBool(v)
	}
	if v := os.Getenv("CODEAGENT_WEB_TOKEN"); v != "" {
		c.WebToken = v
	}
	if v := os.Getenv("CODEAGENT_WEB_INSECURE"); v != "" {
		c.WebInsecure = parseBool(v)
	}
	if v := os.Getenv("CODEAGENT_DIAGNOSTIC_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.DiagnosticTimeout = n
		}
	}
}

func parseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func (c *Config) normalize() {
	c.Approval = strings.ToLower(strings.TrimSpace(c.Approval))
	switch c.Approval {
	case "auto", "readonly", "plan":
	default:
		c.Approval = "ask"
	}
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "anthropic", "claude":
		c.Provider = "anthropic"
	case "gemini", "google":
		c.Provider = "gemini"
	case "ollama":
		c.Provider = "ollama"
	default:
		c.Provider = "openai"
	}
	c.applyProviderPreset()
	if c.BaseURL == "" {
		c.BaseURL = "https://api.deepseek.com"
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.Model == "" {
		c.Model = "deepseek-chat"
	}
	if c.WorkDir == "" {
		c.WorkDir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(c.WorkDir); err == nil {
		c.WorkDir = abs
	}
	if real, err := filepath.EvalSymlinks(c.WorkDir); err == nil {
		c.WorkDir = real
	}
	if c.SessionsDir == "" {
		c.SessionsDir = filepath.Join(c.Home, ".codedeep", "sessions")
	}
	if abs, err := filepath.Abs(c.SessionsDir); err == nil {
		c.SessionsDir = abs
	}
	if c.MCPServers == nil {
		c.MCPServers = map[string]MCPServer{}
	}
	if c.MaxSteps <= 0 {
		c.MaxSteps = 60
	}
	if c.ContextWindow <= 0 {
		c.ContextWindow = 128000
	}
	if c.CompactAt <= 0 || c.CompactAt >= 1 {
		c.CompactAt = 0.8
	}
	for i := range c.Permissions {
		c.Permissions[i].Tool = strings.TrimSpace(c.Permissions[i].Tool)
		c.Permissions[i].Action = strings.ToLower(strings.TrimSpace(c.Permissions[i].Action))
		c.Permissions[i].Command = strings.TrimSpace(c.Permissions[i].Command)
		c.Permissions[i].Path = strings.TrimSpace(c.Permissions[i].Path)
	}
	c.Hooks.PreTool = normalizeHooks(c.Hooks.PreTool)
	c.Hooks.PostTool = normalizeHooks(c.Hooks.PostTool)
	rules := c.Diagnostics[:0]
	for _, rule := range c.Diagnostics {
		rule.Pattern = strings.TrimSpace(rule.Pattern)
		rule.Command = strings.TrimSpace(rule.Command)
		if rule.Pattern == "" || rule.Command == "" {
			continue
		}
		rules = append(rules, rule)
	}
	c.Diagnostics = rules
	if c.DiagnosticTimeout <= 0 {
		c.DiagnosticTimeout = 30
	}
	if c.DiagnosticTimeout > 600 {
		c.DiagnosticTimeout = 600
	}
	c.WebToken = strings.TrimSpace(c.WebToken)
	if c.Editor != nil {
		c.Editor.Command = strings.TrimSpace(c.Editor.Command)
		if c.Editor.Command == "" {
			c.Editor = nil
		} else {
			args := c.Editor.Args[:0]
			for _, arg := range c.Editor.Args {
				if arg != "" {
					args = append(args, arg)
				}
			}
			c.Editor.Args = args
		}
	}
}

func normalizeHooks(in []Hook) []Hook {
	out := in[:0]
	for _, h := range in {
		h.Command = strings.TrimSpace(h.Command)
		if h.Command == "" {
			continue
		}
		if h.TimeoutSec <= 0 {
			h.TimeoutSec = 10
		}
		if h.TimeoutSec > 300 {
			h.TimeoutSec = 300
		}
		tools := h.Tools[:0]
		for _, name := range h.Tools {
			if name = strings.TrimSpace(name); name != "" {
				tools = append(tools, name)
			}
		}
		h.Tools = tools
		out = append(out, h)
	}
	return out
}

// SetProvider validates and selects a provider at runtime, applying the
// provider's preset base URL/model when the current values are still the
// defaults.
func (c *Config) SetProvider(provider string) error {
	p := strings.ToLower(strings.TrimSpace(provider))
	switch p {
	case "openai", "ollama", "anthropic", "gemini":
	case "claude":
		p = "anthropic"
	case "google":
		p = "gemini"
	default:
		return fmt.Errorf("unsupported provider %q (use openai, anthropic, gemini, or ollama)", provider)
	}
	c.Provider = p
	c.applyProviderPreset()
	return nil
}

// applyProviderPreset fills base URL, model, and context window defaults for
// the selected provider. Values explicitly set by the user are preserved: the
// preset only replaces the DeepSeek defaults or empty fields.
func (c *Config) applyProviderPreset() {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	untouchedBase := base == "" || base == "https://api.deepseek.com"
	model := strings.TrimSpace(c.Model)
	untouchedModel := model == "" || model == "deepseek-chat"
	switch c.Provider {
	case "anthropic":
		if untouchedBase {
			c.BaseURL = "https://api.anthropic.com"
		}
		if untouchedModel {
			c.Model = "claude-sonnet-4-5"
		}
		if c.ContextWindow == 128000 {
			c.ContextWindow = 200000
		}
	case "gemini":
		if untouchedBase {
			c.BaseURL = "https://generativelanguage.googleapis.com"
		}
		if untouchedModel {
			c.Model = "gemini-2.5-pro"
		}
		if c.ContextWindow == 128000 {
			c.ContextWindow = 1000000
		}
	case "ollama":
		if untouchedBase {
			c.BaseURL = "http://localhost:11434/v1"
		}
		if untouchedModel {
			c.Model = "qwen2.5-coder:7b"
		}
		if c.ContextWindow == 128000 {
			c.ContextWindow = 32768
		}
	}
}

// APIKeyPresent reports whether a key is configured, regardless of validity.
// Local Ollama servers do not require one.
func (c *Config) APIKeyPresent() bool {
	if c.Provider == "ollama" {
		return true
	}
	return strings.TrimSpace(c.APIKey) != ""
}

// UsageCost estimates the USD cost of a usage record for the current model.
// The boolean is false when no pricing is configured for the model.
func (c *Config) UsageCost(u llm.Usage) (float64, bool) {
	p, ok := c.Pricing[c.Model]
	if !ok || (p.InputPerMTok == 0 && p.OutputPerMTok == 0) {
		return 0, false
	}
	cache := u.PromptCacheHitTokens
	if cache < 0 {
		cache = 0
	}
	if cache > u.PromptTokens {
		cache = u.PromptTokens
	}
	miss := u.PromptTokens - cache
	cost := float64(miss)*p.InputPerMTok/1e6 +
		float64(u.CompletionTokens)*p.OutputPerMTok/1e6
	if p.CacheHitPerMTok > 0 {
		cost += float64(cache) * p.CacheHitPerMTok / 1e6
	} else {
		cost += float64(cache) * p.InputPerMTok / 1e6
	}
	return cost, true
}
