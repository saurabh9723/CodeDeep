package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderAliases(t *testing.T) {
	v := Vars{
		APIKey:           "shared",
		DeepSeekAPIKey:   "deepseek",
		AnthropicAPIKey:  "anthropic",
		GeminiAPIKey:     "gemini",
		DeepSeekBaseURL:  "https://deepseek.test",
		AnthropicBaseURL: "https://anthropic.test",
		GeminiBaseURL:    "https://gemini.test",
		Model:            "primary",
		DeepSeekModel:    "fallback",
	}
	cases := []struct {
		provider string
		key      string
		base     string
	}{
		{"openai", "deepseek", "https://deepseek.test"},
		{"claude", "anthropic", "https://anthropic.test"},
		{"gemini", "gemini", "https://gemini.test"},
		{"ollama", "shared", ""},
	}
	for _, tc := range cases {
		if got := v.KeyFor(tc.provider); got != tc.key {
			t.Errorf("KeyFor(%q) = %q, want %q", tc.provider, got, tc.key)
		}
		if got := v.BaseURLFor(tc.provider); got != tc.base {
			t.Errorf("BaseURLFor(%q) = %q, want %q", tc.provider, got, tc.base)
		}
	}
	if got := v.ModelName(); got != "primary" {
		t.Errorf("ModelName = %q", got)
	}
	if got := (Vars{DeepSeekModel: "only"}).ModelName(); got != "only" {
		t.Errorf("fallback ModelName = %q", got)
	}
}

func TestLoadParsesTypedValues(t *testing.T) {
	t.Setenv("CODEAGENT_MAX_TOKENS", "2048")
	t.Setenv("CODEAGENT_TEMPERATURE", "0.7")
	t.Setenv("CODEAGENT_PROMPT_CACHE", "no")
	t.Setenv("CODEAGENT_DEBUG", "yes")
	t.Setenv("COLUMNS", "120")

	v, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v.MaxTokens == nil || *v.MaxTokens != 2048 {
		t.Errorf("max tokens = %v", v.MaxTokens)
	}
	if v.Temperature == nil || *v.Temperature != 0.7 {
		t.Errorf("temperature = %v", v.Temperature)
	}
	if v.PromptCache == nil || v.PromptCache.Value {
		t.Errorf("prompt cache = %v", v.PromptCache)
	}
	if v.Debug == nil || !v.Debug.Value {
		t.Errorf("debug = %v", v.Debug)
	}
	if v.Columns == nil || *v.Columns != 120 {
		t.Errorf("columns = %v", v.Columns)
	}
}

func TestInvalidValuesNameTheVariable(t *testing.T) {
	t.Setenv("CODEAGENT_DEBUG", "maybe")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CODEAGENT_DEBUG") {
		t.Fatalf("want an error naming CODEAGENT_DEBUG, got %v", err)
	}

	t.Setenv("CODEAGENT_DEBUG", "")
	t.Setenv("CODEAGENT_MAX_STEPS", "lots")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CODEAGENT_MAX_STEPS") {
		t.Fatalf("want an error naming CODEAGENT_MAX_STEPS, got %v", err)
	}
}

func TestDotEnvLoadsAndShellWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("CODEAGENT_SHELL=/bin/zsh-from-dotenv\nCODEAGENT_WEB_ADDR=127.0.0.1:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEAGENT_ENV_FILE", path)
	unsetForTest(t, "CODEAGENT_SHELL")
	unsetForTest(t, "CODEAGENT_WEB_ADDR")

	v, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v.Shell != "/bin/zsh-from-dotenv" || v.WebAddr != "127.0.0.1:9999" {
		t.Fatalf("dotenv values = %q / %q", v.Shell, v.WebAddr)
	}

	t.Setenv("CODEAGENT_SHELL", "/bin/sh-from-shell")
	v, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if v.Shell != "/bin/sh-from-shell" {
		t.Fatalf("shell env should win over dotenv, got %q", v.Shell)
	}
}

func unsetForTest(t *testing.T, name string) {
	t.Helper()
	value, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, value)
			return
		}
		_ = os.Unsetenv(name)
	})
}
