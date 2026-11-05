package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type MCPServer struct {
	Command    string            `json:"command"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Disabled   bool              `json:"disabled,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
}

type Config struct {
	APIKey           string               `json:"api_key"`
	BaseURL          string               `json:"base_url"`
	Model            string               `json:"model"`
	MaxTokens        int                  `json:"max_tokens"`
	Temperature      float64              `json:"temperature"`
	ContextWindow    int                  `json:"context_window"`
	CompactAt        float64              `json:"compact_at"`
	Approval         string               `json:"approval"`
	WorkDir          string               `json:"-"`
	SessionsDir      string               `json:"sessions_dir"`
	Shell            string               `json:"shell"`
	MaxSteps         int                  `json:"max_steps"`
	AllowOutside     bool                 `json:"allow_outside"`
	WebAddr          string               `json:"web_addr"`
	Debug            bool                 `json:"debug"`
	MCPServers       map[string]MCPServer `json:"mcp_servers"`
	InstructionFiles []string             `json:"instruction_files"`
	ConfigPath       string               `json:"-"`
	Home             string               `json:"-"`
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	wd, _ := os.Getwd()
	return &Config{
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
	if v := envFirst("DEEPSEEK_API_KEY", "CODEAGENT_API_KEY", "OPENAI_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := envFirst("DEEPSEEK_BASE_URL", "CODEAGENT_BASE_URL", "OPENAI_BASE_URL"); v != "" {
		c.BaseURL = v
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
	case "auto", "readonly":
	default:
		c.Approval = "ask"
	}
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
}

// APIKeyPresent reports whether a key is configured, regardless of validity.
func (c *Config) APIKeyPresent() bool { return strings.TrimSpace(c.APIKey) != "" }
