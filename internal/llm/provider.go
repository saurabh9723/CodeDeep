package llm

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
)

// Provider is the streaming chat interface used by the agent. The built-in
// OpenAI-compatible Client implements it, as do the Anthropic and Gemini
// adapters.
type Provider interface {
	Stream(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*Response, error)
	Complete(ctx context.Context, req ChatRequest) (*Response, error)
	ListModels(ctx context.Context) ([]string, error)
}

// NewProvider builds a provider from config values. "openai" (also used for
// DeepSeek, Ollama, and other compatible servers) is the default.
func NewProvider(kind, baseURL, apiKey string) Provider {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "anthropic", "claude":
		return NewAnthropicClient(baseURL, apiKey)
	case "gemini", "google":
		return NewGeminiClient(baseURL, apiKey)
	default:
		return NewClient(baseURL, apiKey)
	}
}

// errStopSSE lets stream parsers finish before the transport reaches EOF.
var errStopSSE = errors.New("stop sse")

// readSSE feeds the payload of each data: line to fn. Comment/event lines are
// ignored. SSE payloads are small here; 4 MB is a generous safety cap.
func readSSE(ctx context.Context, body io.ReadCloser, fn func([]byte) error) error {
	defer body.Close()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if err := fn([]byte(payload)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return ctx.Err()
}

func mimeOr(mime, fallback string) string {
	mime = strings.TrimSpace(mime)
	if mime == "" {
		return fallback
	}
	return mime
}
