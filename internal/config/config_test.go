package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codedeep/internal/llm"
)

var configEnvVars = []string{
	"DEEPSEEK_API_KEY", "CODEAGENT_API_KEY", "OPENAI_API_KEY",
	"DEEPSEEK_BASE_URL", "CODEAGENT_BASE_URL", "OPENAI_BASE_URL",
	"CODEAGENT_MODEL", "DEEPSEEK_MODEL",
	"CODEAGENT_MAX_TOKENS", "CODEAGENT_TEMPERATURE", "CODEAGENT_CONTEXT_WINDOW",
	"CODEAGENT_COMPACT_AT", "CODEAGENT_APPROVAL", "CODEAGENT_SESSIONS_DIR",
	"CODEAGENT_SHELL", "CODEAGENT_MAX_STEPS", "CODEAGENT_WEB_ADDR",
	"CODEAGENT_DEBUG", "CODEAGENT_ALLOW_OUTSIDE",
	"CODEAGENT_CONFIG", "CODEAGENT_MCP_CONFIG",
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range configEnvVars {
		t.Setenv(k, "")
	}
}

func loadFrom(t *testing.T, body string) *Config {
	t.Helper()
	clearConfigEnv(t)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestDefaults(t *testing.T) {
	cfg := loadFrom(t, "{}")
	if cfg.Model != "deepseek-chat" {
		t.Errorf("model = %q", cfg.Model)
	}
	if cfg.BaseURL != "https://api.deepseek.com" {
		t.Errorf("base url = %q", cfg.BaseURL)
	}
	if cfg.Approval != "ask" {
		t.Errorf("approval = %q", cfg.Approval)
	}
	if cfg.MaxSteps != 60 || cfg.ContextWindow != 128000 || cfg.CompactAt != 0.8 {
		t.Errorf("loop defaults = %d / %d / %v", cfg.MaxSteps, cfg.ContextWindow, cfg.CompactAt)
	}
	if cfg.MaxTokens != 8192 || cfg.Temperature != 0.3 {
		t.Errorf("model defaults = %d / %v", cfg.MaxTokens, cfg.Temperature)
	}
	if cfg.MCPServers == nil {
		t.Error("MCPServers should be non-nil")
	}
	if cfg.WorkDir == "" {
		t.Error("WorkDir should be resolved")
	}
}

func TestMissingExplicitFileIsAnError(t *testing.T) {
	clearConfigEnv(t)
	missing := filepath.Join(t.TempDir(), "nope.json")
	if _, err := Load(missing); err == nil {
		t.Fatal("Load should fail for an explicit missing file")
	}
}

func TestFileValuesAndCamelCaseMCP(t *testing.T) {
	cfg := loadFrom(t, `{
		"model": "custom-model",
		"approval": "AUTO",
		"mcpServers": {"fs": {"command": "node", "args": ["server.js"]}},
		"mcp_servers": {"other": {"command": "other", "disabled": true}}
	}`)
	if cfg.Model != "custom-model" {
		t.Errorf("model = %q", cfg.Model)
	}
	if cfg.Approval != "auto" {
		t.Errorf("approval = %q, want normalized auto", cfg.Approval)
	}
	fs, ok := cfg.MCPServers["fs"]
	if !ok || fs.Command != "node" || len(fs.Args) != 1 {
		t.Errorf("camelCase mcpServers not loaded: %+v", cfg.MCPServers)
	}
	other, ok := cfg.MCPServers["other"]
	if !ok || !other.Disabled {
		t.Errorf("snake_case mcp_servers not loaded: %+v", cfg.MCPServers)
	}
}

func TestEnvOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")
	t.Setenv("CODEAGENT_MODEL", "env-model")
	t.Setenv("CODEAGENT_BASE_URL", "http://localhost:1234/")
	t.Setenv("CODEAGENT_APPROVAL", "READONLY")
	t.Setenv("CODEAGENT_MAX_STEPS", "7")
	t.Setenv("CODEAGENT_COMPACT_AT", "0.5")
	t.Setenv("CODEAGENT_MAX_TOKENS", "2048")
	t.Setenv("CODEAGENT_DEBUG", "yes")
	t.Setenv("CODEAGENT_ALLOW_OUTSIDE", "on")

	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "sk-test" || !cfg.APIKeyPresent() {
		t.Errorf("api key = %q", cfg.APIKey)
	}
	if cfg.Model != "env-model" {
		t.Errorf("model = %q", cfg.Model)
	}
	if cfg.BaseURL != "http://localhost:1234" {
		t.Errorf("base url = %q, want trailing slash trimmed", cfg.BaseURL)
	}
	if cfg.Approval != "readonly" {
		t.Errorf("approval = %q", cfg.Approval)
	}
	if cfg.MaxSteps != 7 || cfg.CompactAt != 0.5 || cfg.MaxTokens != 2048 {
		t.Errorf("numeric overrides = %d / %v / %d", cfg.MaxSteps, cfg.CompactAt, cfg.MaxTokens)
	}
	if !cfg.Debug || !cfg.AllowOutside {
		t.Errorf("bool overrides = debug:%v outside:%v", cfg.Debug, cfg.AllowOutside)
	}
}

func TestInvalidEnvValuesKeepDefaults(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CODEAGENT_MAX_TOKENS", "abc")
	t.Setenv("CODEAGENT_CONTEXT_WINDOW", "500")
	t.Setenv("CODEAGENT_COMPACT_AT", "1.5")
	t.Setenv("CODEAGENT_APPROVAL", "yolo")
	t.Setenv("CODEAGENT_MAX_STEPS", "-3")

	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxTokens != 8192 || cfg.ContextWindow != 128000 {
		t.Errorf("invalid ints were applied: %d / %d", cfg.MaxTokens, cfg.ContextWindow)
	}
	if cfg.CompactAt != 0.8 || cfg.Approval != "ask" || cfg.MaxSteps != 60 {
		t.Errorf("invalid values were applied: %v / %q / %d", cfg.CompactAt, cfg.Approval, cfg.MaxSteps)
	}
}

func TestNormalizeRepairsOutOfRangeValues(t *testing.T) {
	cfg := loadFrom(t, `{
		"context_window": 0,
		"max_steps": 0,
		"compact_at": 2,
		"base_url": "http://example.test///",
		"model": ""
	}`)
	if cfg.ContextWindow != 128000 || cfg.MaxSteps != 60 {
		t.Errorf("normalize did not repair loop values: %d / %d", cfg.ContextWindow, cfg.MaxSteps)
	}
	if cfg.CompactAt != 0.8 {
		t.Errorf("compact_at = %v", cfg.CompactAt)
	}
	if cfg.BaseURL != "http://example.test" {
		t.Errorf("base url = %q", cfg.BaseURL)
	}
	if cfg.Model != "deepseek-chat" {
		t.Errorf("model = %q", cfg.Model)
	}
}

func TestMCPConfigFile(t *testing.T) {
	clearConfigEnv(t)
	mcpPath := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"mock":{"command":"mockmcp","args":["--x"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEAGENT_MCP_CONFIG", mcpPath)

	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"mcp_servers":{"local":{"command":"local"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.MCPServers["local"]; !ok {
		t.Error("main config server missing")
	}
	mock, ok := cfg.MCPServers["mock"]
	if !ok || mock.Command != "mockmcp" || len(mock.Args) != 1 {
		t.Errorf("MCP config file not merged: %+v", cfg.MCPServers)
	}
}

func TestMCPConfigFileErrors(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CODEAGENT_MCP_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(cfgPath); err == nil || !strings.Contains(err.Error(), "MCP config") {
		t.Fatalf("expected MCP config error, got %v", err)
	}
}

func TestInvalidJSONIsAnError(t *testing.T) {
	clearConfigEnv(t)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestConfigPathRecording(t *testing.T) {
	clearConfigEnv(t)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigPath != p {
		t.Errorf("ConfigPath = %q, want %q", cfg.ConfigPath, p)
	}
}

func TestUsageCostWithoutPricing(t *testing.T) {
	cfg := &Config{Model: "m"}
	if _, ok := cfg.UsageCost(llm.Usage{PromptTokens: 100}); ok {
		t.Fatal("cost should be unavailable without pricing")
	}

	cfg.Pricing = map[string]Price{"other": {InputPerMTok: 1}}
	if _, ok := cfg.UsageCost(llm.Usage{PromptTokens: 100}); ok {
		t.Fatal("cost should be unavailable for an unpriced model")
	}
}

func TestUsageCostInputAndOutput(t *testing.T) {
	cfg := &Config{
		Model: "m",
		Pricing: map[string]Price{
			"m": {InputPerMTok: 2, OutputPerMTok: 8},
		},
	}
	cost, ok := cfg.UsageCost(llm.Usage{
		PromptTokens:     1_000_000,
		CompletionTokens: 500_000,
	})
	if !ok {
		t.Fatal("expected a cost")
	}
	if math.Abs(cost-6) > 1e-9 {
		t.Errorf("cost = %v, want 6", cost)
	}
}

func TestUsageCostCacheHit(t *testing.T) {
	cfg := &Config{
		Model: "m",
		Pricing: map[string]Price{
			"m": {InputPerMTok: 2, OutputPerMTok: 8, CacheHitPerMTok: 0.5},
		},
	}
	cost, _ := cfg.UsageCost(llm.Usage{
		PromptTokens:         1_000_000,
		PromptCacheHitTokens: 400_000,
	})
	if math.Abs(cost-1.4) > 1e-9 {
		t.Errorf("cost = %v, want 1.4", cost)
	}

	// Without a cache price, cached tokens are billed at the input rate.
	cfg.Pricing["m"] = Price{InputPerMTok: 2, OutputPerMTok: 8}
	cost, _ = cfg.UsageCost(llm.Usage{
		PromptTokens:         1_000_000,
		PromptCacheHitTokens: 400_000,
	})
	if math.Abs(cost-2) > 1e-9 {
		t.Errorf("cost without cache price = %v, want 2", cost)
	}
}

func TestUsageCostClampsCacheHits(t *testing.T) {
	cfg := &Config{
		Model: "m",
		Pricing: map[string]Price{
			"m": {InputPerMTok: 1, OutputPerMTok: 1},
		},
	}
	cost, _ := cfg.UsageCost(llm.Usage{
		PromptTokens:         100,
		PromptCacheHitTokens: 500,
	})
	if math.Abs(cost-0.0001) > 1e-12 {
		t.Errorf("cost = %v, want 0.0001", cost)
	}
}

func TestPlanApprovalMode(t *testing.T) {
	cfg := loadFrom(t, `{"approval": "PLAN"}`)
	if cfg.Approval != "plan" {
		t.Fatalf("approval = %q, want plan", cfg.Approval)
	}
}

func TestPricingFile(t *testing.T) {
	cfg := loadFrom(t, `{
		"model": "priced",
		"pricing": {
			"priced": {"input_per_mtok": 0.27, "output_per_mtok": 1.1, "cache_hit_per_mtok": 0.07}
		}
	}`)
	p, ok := cfg.Pricing["priced"]
	if !ok || p.InputPerMTok != 0.27 || p.OutputPerMTok != 1.1 || p.CacheHitPerMTok != 0.07 {
		t.Fatalf("pricing = %+v", cfg.Pricing)
	}
}
