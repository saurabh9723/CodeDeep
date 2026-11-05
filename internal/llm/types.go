package llm

import (
	"encoding/json"
	"fmt"
)

// Message is the transport-neutral chat message used by the agent and stored
// in sessions. Reasoning is kept for display/history; providers that require
// thinking blocks echoed back (Anthropic) also keep the signature.
type Message struct {
	Role               string     `json:"role"`
	Content            string     `json:"content,omitempty"`
	Reasoning          string     `json:"reasoning_content,omitempty"`
	ReasoningSignature string     `json:"reasoning_signature,omitempty"`
	Images             []Image    `json:"images,omitempty"`
	ToolCalls          []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID         string     `json:"tool_call_id,omitempty"`
}

// Image is a base64-encoded user attachment. Data excludes the data-URL
// prefix; MIME is a media type such as image/png.
type Image struct {
	MIME string `json:"mime"`
	Data string `json:"data"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []Tool
	MaxTokens   int
	Temperature float64
	// ReasoningEffort is a provider hint such as minimal, low, medium, or high.
	ReasoningEffort string
	// ReasoningBudget is an extended-thinking token budget for Anthropic and
	// Gemini models.
	ReasoningBudget int
	// PromptCache enables provider-native prompt caching. It is currently
	// explicit only for Anthropic; other providers cache automatically.
	PromptCache bool
}

type Usage struct {
	// PromptTokens is the total input, including cache hits and writes.
	PromptTokens           int `json:"prompt_tokens"`
	CompletionTokens       int `json:"completion_tokens"`
	TotalTokens            int `json:"total_tokens"`
	PromptCacheHitTokens   int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheWriteTokens int `json:"prompt_cache_write_tokens,omitempty"`
}

func (u Usage) Add(o Usage) Usage {
	return Usage{
		PromptTokens:           u.PromptTokens + o.PromptTokens,
		CompletionTokens:       u.CompletionTokens + o.CompletionTokens,
		TotalTokens:            u.TotalTokens + o.TotalTokens,
		PromptCacheHitTokens:   u.PromptCacheHitTokens + o.PromptCacheHitTokens,
		PromptCacheWriteTokens: u.PromptCacheWriteTokens + o.PromptCacheWriteTokens,
	}
}

// ToolUsage attributes model usage to the tool calls a response requested.
// CostUSD is filled by the agent from the configured model pricing.
type ToolUsage struct {
	Calls   int     `json:"calls"`
	Usage   Usage   `json:"usage"`
	CostUSD float64 `json:"cost_usd,omitempty"`
}

type Response struct {
	Message      Message
	FinishReason string
	Usage        Usage
}

// Delta is a streamed fragment of an assistant turn.
type Delta struct {
	Content   string
	Reasoning string
}

type APIError struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("llm: http %d", e.Status)
	}
	if e.Type != "" {
		return fmt.Sprintf("llm: http %d (%s): %s", e.Status, e.Type, e.Message)
	}
	return fmt.Sprintf("llm: http %d: %s", e.Status, e.Message)
}

// EstimatedTokens is a coarse local estimate used for context accounting when
// the provider does not return usage for streamed responses.
func EstimatedTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += 4 // role/framing overhead
		total += len(m.Content) / 4
		total += len(m.Reasoning) / 4
		total += len(m.Images) * 1000
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Name)/4 + len(tc.Function.Arguments)/4 + 8
		}
	}
	return total
}
