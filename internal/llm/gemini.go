package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// GeminiClient speaks the Google Gemini generateContent API, including SSE
// streaming, tool calls, and images.
type GeminiClient struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewGeminiClient(baseURL, apiKey string) *GeminiClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	for _, suffix := range []string{"/v1beta", "/v1"} {
		baseURL = strings.TrimSuffix(baseURL, suffix)
	}
	return &GeminiClient{BaseURL: baseURL, APIKey: apiKey, HTTP: newHTTPClient()}
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	Thought          bool                    `json:"thought,omitempty"`
	InlineData       *geminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	ID   string         `json:"id,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDecl struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	Tools             []geminiTool    `json:"tools,omitempty"`
	GenerationConfig  map[string]any  `json:"generationConfig,omitempty"`
}

func (c *GeminiClient) buildBody(req ChatRequest) ([]byte, error) {
	body := geminiRequest{}
	config := map[string]any{}
	if req.MaxTokens > 0 {
		config["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		config["temperature"] = req.Temperature
	}
	if req.ReasoningBudget > 0 {
		config["thinkingConfig"] = map[string]any{
			"thinkingBudget":  req.ReasoningBudget,
			"includeThoughts": true,
		}
	}
	if len(config) > 0 {
		body.GenerationConfig = config
	}

	var system []string
	for _, m := range req.Messages {
		if m.Role == "system" && strings.TrimSpace(m.Content) != "" {
			system = append(system, m.Content)
		}
	}
	if len(system) > 0 {
		body.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(system, "\n\n")}}}
	}

	toolNames := map[string]string{}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					toolNames[tc.ID] = tc.Function.Name
				}
			}
		}
	}

	for _, m := range req.Messages {
		var role string
		var parts []geminiPart
		switch m.Role {
		case "system":
			continue
		case "user":
			role = "user"
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, img := range m.Images {
				if strings.TrimSpace(img.Data) == "" {
					continue
				}
				parts = append(parts, geminiPart{InlineData: &geminiInlineData{
					MIMEType: mimeOr(img.MIME, "image/png"),
					Data:     img.Data,
				}})
			}
		case "assistant":
			role = "model"
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				var args map[string]any
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil || args == nil {
					args = map[string]any{}
				}
				parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
					Name: tc.Function.Name,
					Args: args,
				}})
			}
		case "tool":
			role = "user"
			name := toolNames[m.ToolCallID]
			if name == "" {
				name = "tool"
			}
			parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResponse{
				Name:     name,
				Response: map[string]any{"result": m.Content},
			}})
		default:
			role = "user"
			if m.Content != "" {
				parts = append(parts, geminiPart{Text: m.Content})
			}
		}
		if len(parts) == 0 {
			continue
		}
		appendGeminiContent(&body.Contents, role, parts)
	}

	for _, t := range req.Tools {
		var params any
		if len(t.Function.Parameters) > 0 {
			var raw any
			if err := json.Unmarshal(t.Function.Parameters, &raw); err == nil {
				params = geminiSchema(raw)
			}
		}
		body.Tools = append(body.Tools, geminiTool{FunctionDeclarations: []geminiFunctionDecl{{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
		}}})
	}
	return json.Marshal(body)
}

// appendGeminiContent merges consecutive same-role turns; Gemini expects an
// alternating user/model conversation.
func appendGeminiContent(contents *[]geminiContent, role string, parts []geminiPart) {
	if n := len(*contents); n > 0 && (*contents)[n-1].Role == role {
		(*contents)[n-1].Parts = append((*contents)[n-1].Parts, parts...)
		return
	}
	*contents = append(*contents, geminiContent{Role: role, Parts: parts})
}

// geminiSchema converts JSON Schema to the OpenAPI subset Gemini accepts:
// uppercase type names and no unsupported keywords.
func geminiSchema(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for key, val := range t {
			switch key {
			case "type":
				if s, ok := val.(string); ok {
					out[key] = strings.ToUpper(s)
				}
			case "additionalProperties", "$schema", "default":
				continue
			default:
				out[key] = geminiSchema(val)
			}
		}
		return out
	case []any:
		for i := range t {
			t[i] = geminiSchema(t[i])
		}
		return t
	default:
		return v
	}
}

func (c *GeminiClient) endpoint(model, method string, stream bool) string {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	u := fmt.Sprintf("%s/v1beta/models/%s:%s", c.BaseURL, url.PathEscape(model), method)
	if stream {
		u += "?alt=sse"
	}
	return u
}

func (c *GeminiClient) newRequest(ctx context.Context, endpoint string, payload []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CodeDeep/0.1")
	if c.APIKey != "" {
		req.Header.Set("x-goog-api-key", c.APIKey)
	}
	return req, nil
}

type geminiUsage struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
}

func (u *geminiUsage) toUsage() Usage {
	if u == nil {
		return Usage{}
	}
	total := u.TotalTokenCount
	if total == 0 {
		total = u.PromptTokenCount + u.CandidatesTokenCount
	}
	return Usage{
		PromptTokens:         u.PromptTokenCount,
		CompletionTokens:     u.CandidatesTokenCount,
		TotalTokens:          total,
		PromptCacheHitTokens: u.CachedContentTokenCount,
	}
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata  *geminiUsage `json:"usageMetadata"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func geminiStopReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return strings.ToLower(reason)
	}
}

func (c *GeminiClient) Stream(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (*Response, error) {
	const maxAttempts = 4
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		payload, err := c.buildBody(req)
		if err != nil {
			return nil, err
		}
		httpReq, err := c.newRequest(ctx, c.endpoint(req.Model, "streamGenerateContent", true), payload)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		httpResp, err := c.HTTP.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt < maxAttempts-1 {
				delay := retryDelay(attempt, nil)
				if nerr := notifyRetry(onDelta, attempt+1, maxAttempts, 0, err, delay); nerr != nil {
					return nil, nerr
				}
				if werr := sleepCtx(ctx, delay); werr != nil {
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
				delay := retryDelay(attempt, httpResp)
				if nerr := notifyRetry(onDelta, attempt+1, maxAttempts, httpResp.StatusCode, apiErr, delay); nerr != nil {
					return nil, nerr
				}
				if werr := sleepCtx(ctx, delay); werr != nil {
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

func (c *GeminiClient) parseStream(ctx context.Context, body io.ReadCloser, onDelta func(Delta) error) (*Response, error) {
	resp := &Response{}
	var text, reasoning strings.Builder
	calls := 0

	err := readSSE(ctx, body, func(data []byte) error {
		var chunk geminiResponse
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("llm: decode gemini stream: %w", err)
		}
		if chunk.Error != nil {
			return &APIError{Status: chunk.Error.Code, Type: chunk.Error.Status, Message: chunk.Error.Message}
		}
		if chunk.UsageMetadata != nil {
			resp.Usage = chunk.UsageMetadata.toUsage()
		}
		for _, cand := range chunk.Candidates {
			if cand.FinishReason != "" {
				resp.FinishReason = geminiStopReason(cand.FinishReason)
			}
			for _, part := range cand.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					args := []byte("{}")
					if part.FunctionCall.Args != nil {
						if b, err := json.Marshal(part.FunctionCall.Args); err == nil {
							args = b
						}
					}
					calls++
					id := strings.TrimSpace(part.FunctionCall.ID)
					if id == "" {
						id = fmt.Sprintf("gemini_call_%d", calls)
					}
					resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
						ID:       id,
						Type:     "function",
						Function: FunctionCall{Name: part.FunctionCall.Name, Arguments: string(args)},
					})
				case part.Thought:
					if part.Text != "" {
						reasoning.WriteString(part.Text)
						if onDelta != nil {
							if err := onDelta(Delta{Reasoning: part.Text}); err != nil {
								return err
							}
						}
					}
				case part.Text != "":
					text.WriteString(part.Text)
					if onDelta != nil {
						if err := onDelta(Delta{Content: part.Text}); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return resp, err
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = text.String()
	resp.Message.Reasoning = reasoning.String()
	if len(resp.Message.ToolCalls) > 0 {
		resp.FinishReason = "tool_calls"
	}
	return resp, nil
}

func (c *GeminiClient) Complete(ctx context.Context, req ChatRequest) (*Response, error) {
	payload, err := c.buildBody(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		httpReq, err := c.newRequest(ctx, c.endpoint(req.Model, "generateContent", false), payload)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Accept", "application/json")
		httpResp, err := c.HTTP.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt < 2 {
				if werr := sleepCtx(ctx, retryDelay(attempt, nil)); werr != nil {
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
				if werr := sleepCtx(ctx, retryDelay(attempt, httpResp)); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, apiErr
		}
		var out geminiResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("llm: decode response: %w", err)
		}
		if out.Error != nil {
			return nil, &APIError{Status: out.Error.Code, Type: out.Error.Status, Message: out.Error.Message}
		}
		resp := &Response{Usage: out.UsageMetadata.toUsage()}
		resp.Message.Role = "assistant"
		var text, reasoning strings.Builder
		if len(out.Candidates) > 0 {
			cand := out.Candidates[0]
			resp.FinishReason = geminiStopReason(cand.FinishReason)
			for i, part := range cand.Content.Parts {
				switch {
				case part.FunctionCall != nil:
					args := []byte("{}")
					if part.FunctionCall.Args != nil {
						if b, err := json.Marshal(part.FunctionCall.Args); err == nil {
							args = b
						}
					}
					id := strings.TrimSpace(part.FunctionCall.ID)
					if id == "" {
						id = fmt.Sprintf("gemini_call_%d", i+1)
					}
					resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{
						ID:       id,
						Type:     "function",
						Function: FunctionCall{Name: part.FunctionCall.Name, Arguments: string(args)},
					})
				case part.Thought:
					reasoning.WriteString(part.Text)
				default:
					text.WriteString(part.Text)
				}
			}
			if len(resp.Message.ToolCalls) > 0 {
				resp.FinishReason = "tool_calls"
			}
		}
		resp.Message.Content = text.String()
		resp.Message.Reasoning = reasoning.String()
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("llm: exhausted retries")
}

func (c *GeminiClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1beta/models?pageSize=1000", nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		httpReq.Header.Set("x-goog-api-key", c.APIKey)
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
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, strings.TrimPrefix(m.Name, "models/"))
	}
	sort.Strings(names)
	return names, nil
}
