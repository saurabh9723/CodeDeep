package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const anthropicVersion = "2023-06-01"

// AnthropicClient speaks the Anthropic Messages API, including streaming and
// tool use.
type AnthropicClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewAnthropicClient(baseURL, apiKey string) *AnthropicClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	return &AnthropicClient{BaseURL: baseURL, APIKey: apiKey, HTTP: newHTTPClient()}
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Temperature *float64           `json:"temperature,omitempty"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	System      any                `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

// anthropicCacheControl marks a prompt prefix for Anthropic's prompt cache.
// Anthropic allows up to four breakpoints per request.
type anthropicCacheControl struct {
	Type string `json:"type"`
}

var ephemeralCache = &anthropicCacheControl{Type: "ephemeral"}

type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Thinking     string                 `json:"thinking,omitempty"`
	Signature    string                 `json:"signature,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        json.RawMessage        `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      string                 `json:"content,omitempty"`
	Source       *anthropicImageSource  `json:"source,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

func (c *AnthropicClient) buildBody(req ChatRequest, stream bool) ([]byte, error) {
	body := anthropicRequest{Model: req.Model, MaxTokens: req.MaxTokens, Stream: stream}
	if body.MaxTokens <= 0 {
		body.MaxTokens = 4096
	}
	if req.ReasoningBudget > 0 {
		body.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: req.ReasoningBudget}
		if body.MaxTokens <= req.ReasoningBudget {
			body.MaxTokens = req.ReasoningBudget + 4096
		}
		// Extended thinking requires the default temperature.
	} else if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}

	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" && strings.TrimSpace(m.Content) != "" {
			system = append(system, m.Content)
		}
	}
	systemText := strings.Join(system, "\n\n")
	if systemText != "" {
		if req.PromptCache {
			body.System = []anthropicSystemBlock{{Type: "text", Text: systemText, CacheControl: ephemeralCache}}
		} else {
			body.System = systemText
		}
	}

	for _, m := range req.Messages {
		var role string
		var blocks []anthropicBlock
		switch m.Role {
		case "system":
			continue
		case "user":
			role = "user"
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, img := range m.Images {
				if strings.TrimSpace(img.Data) == "" {
					continue
				}
				blocks = append(blocks, anthropicBlock{
					Type:   "image",
					Source: &anthropicImageSource{Type: "base64", MediaType: mimeOr(img.MIME, "image/png"), Data: img.Data},
				})
			}
		case "assistant":
			role = "assistant"
			// Extended thinking blocks must lead the assistant turn and keep
			// their signature, or the API rejects the tool-result follow-up.
			// They are only valid while thinking is enabled.
			if req.ReasoningBudget > 0 && m.Reasoning != "" && m.ReasoningSignature != "" {
				blocks = append(blocks, anthropicBlock{Type: "thinking", Thinking: m.Reasoning, Signature: m.ReasoningSignature})
			}
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if len(bytes.TrimSpace(input)) == 0 || !json.Valid(input) {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
		case "tool":
			role = "user"
			blocks = append(blocks, anthropicBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content})
		default:
			role = "user"
			if m.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
			}
		}
		if len(blocks) == 0 {
			continue
		}
		appendAnthropicMessage(&body.Messages, role, blocks)
	}

	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	if req.PromptCache {
		// Cache the tool and system prefixes once, then move the final
		// breakpoint with the conversation so each turn reuses the last one.
		if n := len(body.Tools); n > 0 {
			body.Tools[n-1].CacheControl = ephemeralCache
		}
		if n := len(body.Messages); n > 0 {
			if blocks, ok := body.Messages[n-1].Content.([]anthropicBlock); ok && len(blocks) > 0 {
				blocks[len(blocks)-1].CacheControl = ephemeralCache
				body.Messages[n-1].Content = blocks
			}
		}
	}
	return json.Marshal(body)
}

// appendAnthropicMessage merges adjacent same-role messages, which the
// Messages API expects to alternate.
func appendAnthropicMessage(msgs *[]anthropicMessage, role string, blocks []anthropicBlock) {
	if n := len(*msgs); n > 0 && (*msgs)[n-1].Role == role {
		if prev, ok := (*msgs)[n-1].Content.([]anthropicBlock); ok {
			(*msgs)[n-1].Content = append(prev, blocks...)
			return
		}
	}
	*msgs = append(*msgs, anthropicMessage{Role: role, Content: blocks})
}

func (c *AnthropicClient) newRequest(ctx context.Context, payload []byte, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "CodeDeep/0.1")
	req.Header.Set("anthropic-version", anthropicVersion)
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	return req, nil
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *anthropicBlock `json:"content_block"`
	Delta        *anthropicDelta `json:"delta"`
	Usage        *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *AnthropicClient) Stream(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*Response, error) {
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		payload, err := c.buildBody(req, true)
		if err != nil {
			return nil, err
		}
		httpReq, err := c.newRequest(ctx, payload, "text/event-stream")
		if err != nil {
			return nil, err
		}
		httpResp, err := c.HTTP.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt < maxAttempts-1 {
				if werr := sleepCtx(ctx, backoff(attempt)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, fmt.Errorf("llm: request failed: %w", err)
		}
		if httpResp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 64<<10))
			httpResp.Body.Close()
			apiErr := parseAPIError(httpResp.StatusCode, data)
			if retryableStatus(httpResp.StatusCode) && attempt < maxAttempts-1 {
				lastErr = apiErr
				if werr := sleepCtx(ctx, backoff(attempt)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, apiErr
		}
		return c.parseStream(ctx, httpResp.Body, onDelta)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("llm: exhausted retries")
}

func (c *AnthropicClient) parseStream(ctx context.Context, body io.ReadCloser, onDelta func(Delta) error) (*Response, error) {
	resp := &Response{}
	var text, reasoning, signature strings.Builder
	calls := map[int]*partialCall{}
	var order []int

	err := readSSE(ctx, body, func(data []byte) error {
		var ev anthropicStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("llm: decode anthropic stream: %w", err)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				u := ev.Message.Usage
				resp.Usage.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				resp.Usage.PromptCacheHitTokens = u.CacheReadInputTokens
				resp.Usage.PromptCacheWriteTokens = u.CacheCreationInputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				p := &partialCall{id: ev.ContentBlock.ID, typ: "function"}
				p.name.WriteString(ev.ContentBlock.Name)
				if input := strings.TrimSpace(string(ev.ContentBlock.Input)); input != "" && input != "{}" {
					p.args.WriteString(input)
				}
				calls[ev.Index] = p
				order = append(order, ev.Index)
			}
		case "content_block_delta":
			if ev.Delta == nil {
				break
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					text.WriteString(ev.Delta.Text)
					if onDelta != nil {
						if err := onDelta(Delta{Content: ev.Delta.Text}); err != nil {
							return err
						}
					}
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					reasoning.WriteString(ev.Delta.Thinking)
					if onDelta != nil {
						if err := onDelta(Delta{Reasoning: ev.Delta.Thinking}); err != nil {
							return err
						}
					}
				}
			case "signature_delta":
				signature.WriteString(ev.Delta.Signature)
			case "input_json_delta":
				if p, ok := calls[ev.Index]; ok {
					p.args.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				resp.FinishReason = anthropicStopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				resp.Usage.CompletionTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			return errStopSSE
		case "error":
			if ev.Error != nil {
				return &APIError{Status: http.StatusOK, Type: ev.Error.Type, Message: ev.Error.Message}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopSSE) {
		return resp, err
	}

	resp.Message.Role = "assistant"
	resp.Message.Content = text.String()
	resp.Message.Reasoning = reasoning.String()
	resp.Message.ReasoningSignature = signature.String()
	sort.Ints(order)
	for _, idx := range order {
		p := calls[idx]
		args := strings.TrimSpace(p.args.String())
		if args == "" {
			args = "{}"
		}
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
			ID:       p.id,
			Type:     "function",
			Function: FunctionCall{Name: p.name.String(), Arguments: args},
		})
	}
	if resp.Usage.TotalTokens == 0 {
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	if resp.FinishReason == "" && len(resp.Message.ToolCalls) > 0 {
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

func (c *AnthropicClient) Complete(ctx context.Context, req ChatRequest) (*Response, error) {
	payload, err := c.buildBody(req, false)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		httpReq, err := c.newRequest(ctx, payload, "application/json")
		if err != nil {
			return nil, err
		}
		httpResp, err := c.HTTP.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt < 2 {
				if werr := sleepCtx(ctx, backoff(attempt)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, fmt.Errorf("llm: request failed: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(httpResp.Body, 16<<20))
		httpResp.Body.Close()
		if httpResp.StatusCode >= 300 {
			apiErr := parseAPIError(httpResp.StatusCode, data)
			if retryableStatus(httpResp.StatusCode) && attempt < 2 {
				lastErr = apiErr
				if werr := sleepCtx(ctx, backoff(attempt)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, apiErr
		}
		var out struct {
			Content    []anthropicBlock `json:"content"`
			StopReason string           `json:"stop_reason"`
			Usage      struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("llm: decode response: %w", err)
		}
		if out.Error != nil {
			return nil, &APIError{Status: httpResp.StatusCode, Type: out.Error.Type, Message: out.Error.Message}
		}
		resp := &Response{FinishReason: anthropicStopReason(out.StopReason)}
		resp.Message.Role = "assistant"
		var text, reasoning, signature strings.Builder
		for _, block := range out.Content {
			switch block.Type {
			case "text":
				text.WriteString(block.Text)
			case "thinking":
				reasoning.WriteString(block.Thinking)
				if block.Signature != "" {
					signature.Reset()
					signature.WriteString(block.Signature)
				}
			case "tool_use":
				args := strings.TrimSpace(string(block.Input))
				if args == "" {
					args = "{}"
				}
				resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
					ID:       block.ID,
					Type:     "function",
					Function: FunctionCall{Name: block.Name, Arguments: args},
				})
			}
		}
		resp.Message.Content = text.String()
		resp.Message.Reasoning = reasoning.String()
		resp.Message.ReasoningSignature = signature.String()
		promptTokens := out.Usage.InputTokens + out.Usage.CacheReadInputTokens + out.Usage.CacheCreationInputTokens
		resp.Usage = Usage{
			PromptTokens:           promptTokens,
			CompletionTokens:       out.Usage.OutputTokens,
			TotalTokens:            promptTokens + out.Usage.OutputTokens,
			PromptCacheHitTokens:   out.Usage.CacheReadInputTokens,
			PromptCacheWriteTokens: out.Usage.CacheCreationInputTokens,
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("llm: exhausted retries")
}

func (c *AnthropicClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models?limit=1000", nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if c.APIKey != "" {
		httpReq.Header.Set("x-api-key", c.APIKey)
	}
	httpReq.Header.Set("User-Agent", "CodeDeep/0.1")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, parseAPIError(resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		names = append(names, m.ID)
	}
	sort.Strings(names)
	return names, nil
}
