// Package env is the single place where CodeDeep reads process environment
// variables. A .env file in the working directory (or CODEAGENT_ENV_FILE) is
// loaded first; real environment variables always win over its values.
package env

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	envlib "github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Vars is the typed schema for every environment variable the application
// reads. Optional numeric settings use pointers so an unset variable can be
// distinguished from a deliberate zero value.
type Vars struct {
	ConfigFile    string `env:"CODEAGENT_CONFIG"`
	MCPConfigFile string `env:"CODEAGENT_MCP_CONFIG"`

	Provider string `env:"CODEAGENT_PROVIDER"`
	APIKey   string `env:"CODEAGENT_API_KEY"`
	BaseURL  string `env:"CODEAGENT_BASE_URL"`
	Model    string `env:"CODEAGENT_MODEL"`

	DeepSeekAPIKey  string `env:"DEEPSEEK_API_KEY"`
	OpenAIAPIKey    string `env:"OPENAI_API_KEY"`
	AnthropicAPIKey string `env:"ANTHROPIC_API_KEY"`
	GeminiAPIKey    string `env:"GEMINI_API_KEY"`
	GoogleAPIKey    string `env:"GOOGLE_API_KEY"`

	DeepSeekBaseURL  string `env:"DEEPSEEK_BASE_URL"`
	OpenAIBaseURL    string `env:"OPENAI_BASE_URL"`
	AnthropicBaseURL string `env:"ANTHROPIC_BASE_URL"`
	GeminiBaseURL    string `env:"GEMINI_BASE_URL"`
	GoogleBaseURL    string `env:"GOOGLE_BASE_URL"`
	OllamaBaseURL    string `env:"OLLAMA_BASE_URL"`

	DeepSeekModel string `env:"DEEPSEEK_MODEL"`

	MaxTokens         *int     `env:"CODEAGENT_MAX_TOKENS"`
	Temperature       *float64 `env:"CODEAGENT_TEMPERATURE"`
	ReasoningEffort   string   `env:"CODEAGENT_REASONING_EFFORT"`
	ReasoningBudget   *int     `env:"CODEAGENT_REASONING_BUDGET"`
	PromptCache       *Bool    `env:"CODEAGENT_PROMPT_CACHE"`
	ContextWindow     *int     `env:"CODEAGENT_CONTEXT_WINDOW"`
	CompactAt         *float64 `env:"CODEAGENT_COMPACT_AT"`
	Approval          string   `env:"CODEAGENT_APPROVAL"`
	SessionsDir       string   `env:"CODEAGENT_SESSIONS_DIR"`
	TracesDir         string   `env:"CODEAGENT_TRACES_DIR"`
	Shell             string   `env:"CODEAGENT_SHELL"`
	MaxSteps          *int     `env:"CODEAGENT_MAX_STEPS"`
	WebAddr           string   `env:"CODEAGENT_WEB_ADDR"`
	Debug             *Bool    `env:"CODEAGENT_DEBUG"`
	AllowOutside      *Bool    `env:"CODEAGENT_ALLOW_OUTSIDE"`
	WebToken          string   `env:"CODEAGENT_WEB_TOKEN"`
	WebInsecure       *Bool    `env:"CODEAGENT_WEB_INSECURE"`
	DiagnosticTimeout *int     `env:"CODEAGENT_DIAGNOSTIC_TIMEOUT"`

	Term    string `env:"TERM"`
	CI      string `env:"CI"`
	NoColor string `env:"NO_COLOR"`
	Columns *int   `env:"COLUMNS"`
	Visual  string `env:"VISUAL"`
	Editor  string `env:"EDITOR"`
}

// Bool accepts the same human-friendly values the agent always supported.
type Bool struct {
	Value bool
}

func (b *Bool) UnmarshalText(text []byte) error {
	switch strings.ToLower(strings.TrimSpace(string(text))) {
	case "1", "true", "yes", "on":
		b.Value = true
	case "0", "false", "no", "off":
		b.Value = false
	default:
		return fmt.Errorf("invalid boolean %q (use true/false, yes/no, on/off, or 1/0)", text)
	}
	return nil
}

// Load reads the dotenv file and then parses the process environment into Vars.
// It is safe to call repeatedly; dotenv values never override real variables.
func Load() (Vars, error) {
	if err := loadDotEnv(); err != nil {
		return Vars{}, err
	}
	vars, err := envlib.ParseAs[Vars]()
	if err != nil {
		return Vars{}, describeParseError(err)
	}
	return vars, nil
}

func describeParseError(err error) error {
	var parseErr envlib.ParseError
	if errors.As(err, &parseErr) {
		name := parseErr.Name
		if field, ok := reflect.TypeOf(Vars{}).FieldByName(parseErr.Name); ok {
			if tag := field.Tag.Get("env"); tag != "" {
				name = strings.Split(tag, ",")[0]
			}
		}
		return fmt.Errorf("parse environment: %s: %w", name, parseErr.Err)
	}
	return fmt.Errorf("parse environment: %w", err)
}

// Get returns the parsed environment and treats parse errors as unset values.
// CLI presentation helpers use it where a malformed override should not abort
// the command.
func Get() Vars {
	vars, err := Load()
	if err != nil {
		return Vars{}
	}
	return vars
}

func loadDotEnv() error {
	path := strings.TrimSpace(os.Getenv("CODEAGENT_ENV_FILE"))
	if path == "" {
		path = ".env"
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat env file %s: %w", path, err)
	}
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("load env file %s: %w", path, err)
	}
	return nil
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "anthropic"
	case "gemini", "google":
		return "gemini"
	case "ollama":
		return "ollama"
	default:
		return "openai"
	}
}

// KeyFor returns the first configured API key for a provider, honoring the
// shared CODEAGENT_API_KEY fallback.
func (v Vars) KeyFor(provider string) string {
	switch normalizeProvider(provider) {
	case "anthropic":
		return firstNonEmpty(v.AnthropicAPIKey, v.APIKey)
	case "gemini":
		return firstNonEmpty(v.GeminiAPIKey, v.GoogleAPIKey, v.APIKey)
	case "ollama":
		return firstNonEmpty(v.APIKey)
	default:
		return firstNonEmpty(v.DeepSeekAPIKey, v.APIKey, v.OpenAIAPIKey)
	}
}

// BaseURLFor returns the first configured base URL for a provider, honoring
// the shared CODEAGENT_BASE_URL fallback.
func (v Vars) BaseURLFor(provider string) string {
	switch normalizeProvider(provider) {
	case "anthropic":
		return firstNonEmpty(v.AnthropicBaseURL, v.BaseURL)
	case "gemini":
		return firstNonEmpty(v.GeminiBaseURL, v.GoogleBaseURL, v.BaseURL)
	case "ollama":
		return firstNonEmpty(v.OllamaBaseURL, v.BaseURL)
	default:
		return firstNonEmpty(v.DeepSeekBaseURL, v.BaseURL, v.OpenAIBaseURL)
	}
}

// ModelName returns the configured model, preferring CODEAGENT_MODEL over the
// DeepSeek-specific alias.
func (v Vars) ModelName() string {
	return firstNonEmpty(v.Model, v.DeepSeekModel)
}

// ProviderName returns the configured provider name without validation.
func (v Vars) ProviderName() string {
	return strings.TrimSpace(v.Provider)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
