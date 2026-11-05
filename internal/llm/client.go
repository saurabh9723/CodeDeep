package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Client talks to any OpenAI-compatible chat-completions endpoint. Defaults
// target DeepSeek.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	noStreamUsage atomic.Bool
}

func NewClient(baseURL, apiKey string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 4 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          16,
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Transport: transport},
	}
}

type apiChatRequest struct {
	Model         string         `json:"model"`
	Messages      []apiMessage   `json:"messages"`
	Tools         []Tool         `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type apiMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func toAPIMessages(msgs []Message) []apiMessage {
	out := make([]apiMessage, 0, len(msgs))
	for _, m := range msgs {
		am := apiMessage{Role: m.Role, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
		// An assistant message that only carries tool calls may omit content.
		// Tool results must always carry a (possibly empty) content field.
		if m.Content != "" || m.Role != "assistant" || len(m.ToolCalls) == 0 {
			content := m.Content
			am.Content = &content
		}
		out = append(out, am)
	}
	return out
}

func (c *Client) buildBody(req ChatRequest, stream bool) ([]byte, error) {
	body := apiChatRequest{
		Model:     req.Model,
		Messages:  toAPIMessages(req.Messages),
		Tools:     req.Tools,
		MaxTokens: req.MaxTokens,
		Stream:    stream,
	}
	if req.Temperature > 0 {
		t := req.Temperature
		body.Temperature = &t
	}
	if stream && !c.noStreamUsage.Load() {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return json.Marshal(body)
}

func (c *Client) endpoint() string {
	return c.BaseURL + "/chat/completions"
}

func (c *Client) newRequest(ctx context.Context, payload []byte, accept string) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", "CodeDeep/0.1")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return httpReq, nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Stream sends a chat request and streams text/reasoning deltas. Tool calls are
// accumulated internally and returned in the Response.
func (c *Client) Stream(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*Response, error) {
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
			// Some compatible servers reject stream_options. Drop it and retry
			// once, without consuming a retry slot.
			if httpResp.StatusCode == http.StatusBadRequest && !c.noStreamUsage.Load() &&
				strings.Contains(strings.ToLower(string(data)), "stream_options") {
				c.noStreamUsage.Store(true)
				attempt--
				continue
			}
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

func backoff(attempt int) time.Duration {
	d := time.Duration(800*(1<<attempt)) * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

type apiToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type apiDelta struct {
	Role             string             `json:"role"`
	Content          string             `json:"content"`
	ReasoningContent string             `json:"reasoning_content"`
	ToolCalls        []apiToolCallDelta `json:"tool_calls"`
}

type apiChoice struct {
	Index   int      `json:"index"`
	Delta   apiDelta `json:"delta"`
	Message struct {
		Role             string     `json:"role"`
		Content          string     `json:"content"`
		ReasoningContent string     `json:"reasoning_content"`
		ToolCalls        []ToolCall `json:"tool_calls"`
	} `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

type apiUsage struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	TotalTokens          int `json:"total_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
}

func (u *apiUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		PromptTokens:         u.PromptTokens,
		CompletionTokens:     u.CompletionTokens,
		TotalTokens:          u.TotalTokens,
		PromptCacheHitTokens: u.PromptCacheHitTokens,
	}
}

type apiErrorBody struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func parseAPIError(status int, data []byte) *APIError {
	e := &APIError{Status: status, Message: strings.TrimSpace(string(data))}
	var body apiErrorBody
	if err := json.Unmarshal(data, &body); err == nil && body.Error != nil {
		e.Message = body.Error.Message
		e.Type = body.Error.Type
		if s, ok := body.Error.Code.(string); ok {
			e.Code = s
		}
	}
	if len(e.Message) > 1200 {
		e.Message = e.Message[:1200] + "…"
	}
	return e
}

type apiChunk struct {
	Choices []apiChoice `json:"choices"`
	Usage   *apiUsage   `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

type partialCall struct {
	id   string
	typ  string
	name strings.Builder
	args strings.Builder
}

func (c *Client) parseStream(ctx context.Context, body io.ReadCloser, onDelta func(Delta) error) (*Response, error) {
	defer body.Close()
	resp := &Response{}
	var text, reasoning strings.Builder
	calls := map[int]*partialCall{}
	var order []int

	reader := bufio.NewReaderSize(body, 256<<10)
	done := false
	for !done {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				break
			}
			if payload != "" {
				var ch apiChunk
				if err := json.Unmarshal([]byte(payload), &ch); err != nil {
					return resp, fmt.Errorf("llm: decode stream chunk: %w", err)
				}
				if ch.Error != nil {
					return resp, &APIError{Status: 200, Type: ch.Error.Type, Message: ch.Error.Message}
				}
				if ch.Usage != nil {
					resp.Usage = ch.Usage.toUsage()
				}
				for _, choice := range ch.Choices {
					d := choice.Delta
					if d.ReasoningContent != "" {
						reasoning.WriteString(d.ReasoningContent)
						if onDelta != nil {
							if err := onDelta(Delta{Reasoning: d.ReasoningContent}); err != nil {
								return resp, err
							}
						}
					}
					if d.Content != "" {
						text.WriteString(d.Content)
						if onDelta != nil {
							if err := onDelta(Delta{Content: d.Content}); err != nil {
								return resp, err
							}
						}
					}
					for _, tc := range d.ToolCalls {
						p, ok := calls[tc.Index]
						if !ok {
							p = &partialCall{}
							calls[tc.Index] = p
							order = append(order, tc.Index)
						}
						if tc.ID != "" {
							p.id = tc.ID
						}
						if tc.Type != "" {
							p.typ = tc.Type
						}
						p.name.WriteString(tc.Function.Name)
						p.args.WriteString(tc.Function.Arguments)
					}
					if choice.FinishReason != nil && *choice.FinishReason != "" {
						resp.FinishReason = *choice.FinishReason
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			if ctx.Err() != nil {
				return resp, ctx.Err()
			}
			return resp, fmt.Errorf("llm: read stream: %w", readErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return resp, err
	}

	sort.Ints(order)
	for _, idx := range order {
		p := calls[idx]
		typ := p.typ
		if typ == "" {
			typ = "function"
		}
		name := p.name.String()
		if name == "" {
			continue
		}
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
			ID:   p.id,
			Type: typ,
			Function: FunctionCall{
				Name:      name,
				Arguments: p.args.String(),
			},
		})
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = text.String()
	resp.Message.Reasoning = reasoning.String()
	if resp.FinishReason == "" && len(resp.Message.ToolCalls) > 0 {
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}

// Complete performs a non-streaming completion. Used for compaction and other
// auxiliary calls.
func (c *Client) Complete(ctx context.Context, req ChatRequest) (*Response, error) {
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
			Choices []apiChoice `json:"choices"`
			Usage   *apiUsage   `json:"usage"`
			Error   *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("llm: decode response: %w", err)
		}
		if out.Error != nil {
			return nil, &APIError{Status: httpResp.StatusCode, Type: out.Error.Type, Message: out.Error.Message}
		}
		if len(out.Choices) == 0 {
			return nil, fmt.Errorf("llm: response contained no choices")
		}
		ch := out.Choices[0]
		resp := &Response{
			Message: Message{
				Role:      "assistant",
				Content:   ch.Message.Content,
				Reasoning: ch.Message.ReasoningContent,
				ToolCalls: ch.Message.ToolCalls,
			},
			Usage: out.Usage.toUsage(),
		}
		if ch.FinishReason != nil {
			resp.FinishReason = *ch.FinishReason
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("llm: exhausted retries")
}

// ListModels verifies connectivity and credentials.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
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
