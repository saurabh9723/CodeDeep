package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codedeep/internal/config"
)

type Server struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Timeout time.Duration
	Logf    func(format string, args ...any)
}

type Client struct {
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	stderr  *cappedBuffer
	timeout time.Duration
	logf    func(format string, args ...any)

	writeMu sync.Mutex
	pendMu  sync.Mutex
	pending map[int64]chan rpcReply
	nextID  atomic.Int64

	done     chan struct{}
	doneOnce sync.Once
	readErr  error
	readMu   sync.Mutex

	closeOnce sync.Once
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message)
}

type rpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type Capabilities struct {
	Tools *struct{} `json:"tools,omitempty"`
}

func defaultLogf(string, ...any) {}

// Start launches an MCP stdio server and performs the initialize handshake.
func Start(ctx context.Context, srv Server) (*Client, error) {
	if strings.TrimSpace(srv.Command) == "" {
		return nil, fmt.Errorf("MCP server %q has no command", srv.Name)
	}
	if srv.Timeout <= 0 {
		srv.Timeout = 30 * time.Second
	}
	if srv.Logf == nil {
		srv.Logf = defaultLogf
	}

	cmd := exec.Command(srv.Command, srv.Args...)
	env := os.Environ()
	for k, v := range srv.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &cappedBuffer{max: 16 * 1024}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server %q: %w", srv.Name, err)
	}

	c := &Client{
		name:    srv.Name,
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReaderSize(stdout, 256*1024),
		stderr:  stderr,
		timeout: srv.Timeout,
		logf:    srv.Logf,
		pending: map[int64]chan rpcReply{},
		done:    make(chan struct{}),
	}
	go c.readLoop()

	initCtx, cancel := context.WithTimeout(ctx, srv.Timeout)
	defer cancel()
	if err := c.initialize(initCtx); err != nil {
		c.Close()
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("MCP server %q: %w (stderr: %s)", srv.Name, err, msg)
		}
		return nil, fmt.Errorf("MCP server %q: %w", srv.Name, err)
	}
	srv.Logf("mcp: connected to %q", srv.Name)
	return c, nil
}

func (c *Client) Name() string { return c.name }

func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "CodeDeep",
			"version": "0.1.0",
		},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return err
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		return err
	}
	return nil
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}
	return out.Tools, nil
}

type CallResult struct {
	Output  string
	IsError bool
}

func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	var argValue any
	if err := json.Unmarshal(args, &argValue); err != nil {
		return CallResult{}, fmt.Errorf("invalid arguments: %w", err)
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": argValue,
	})
	if err != nil {
		return CallResult{}, err
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mimeType"`
			Resource *struct {
				Text string `json:"text"`
				URI  string `json:"uri"`
			} `json:"resource"`
		} `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return CallResult{Output: string(raw)}, nil
	}
	var parts []string
	for _, item := range res.Content {
		switch {
		case item.Type == "text" && item.Text != "":
			parts = append(parts, item.Text)
		case item.Resource != nil && item.Resource.Text != "":
			parts = append(parts, item.Resource.Text)
		case item.Type == "image" || item.Type == "audio":
			parts = append(parts, "["+item.Type+" content omitted]")
		case item.Type != "":
			parts = append(parts, "["+item.Type+" content]")
		}
	}
	if len(parts) == 0 && len(res.StructuredContent) > 0 {
		parts = append(parts, string(res.StructuredContent))
	}
	if len(parts) == 0 {
		parts = append(parts, string(raw))
	}
	return CallResult{Output: strings.Join(parts, "\n"), IsError: res.IsError}, nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	id := c.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	req := rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}
	if err := c.send(req); err != nil {
		return nil, err
	}

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.exitError()
	case <-timer.C:
		return nil, fmt.Errorf("MCP %s timed out after %s", method, c.timeout)
	case reply := <-ch:
		if reply.Error != nil {
			return nil, reply.Error
		}
		return reply.Result, nil
	}
}

func (c *Client) notify(method string, params any) error {
	req := rpcRequest{JSONRPC: "2.0", Method: method, Params: params}
	return c.send(req)
}

func (c *Client) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("MCP write failed: %w", err)
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		line, err := c.reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			if err != io.EOF {
				c.fail(fmt.Errorf("MCP read failed: %w", err))
			} else {
				c.fail(fmt.Errorf("MCP server closed stdout"))
			}
			return
		}
	}
}

func (c *Client) handleLine(line []byte) {
	var msg rpcReply
	if err := json.Unmarshal(line, &msg); err != nil {
		c.logf("mcp[%s]: non-JSON output: %s", c.name, strings.TrimSpace(string(line)))
		return
	}
	if msg.Method != "" {
		// Server-initiated request or notification.
		if len(msg.ID) > 0 && string(msg.ID) != "null" {
			reply := rpcResponse{
				JSONRPC: "2.0",
				ID:      msg.ID,
				Error:   &rpcError{Code: -32601, Message: "method not supported by client: " + msg.Method},
			}
			if err := c.send(reply); err != nil {
				c.logf("mcp[%s]: %v", c.name, err)
			}
		}
		return
	}
	var id int64
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	c.pendMu.Lock()
	ch, ok := c.pending[id]
	c.pendMu.Unlock()
	if ok {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (c *Client) fail(err error) {
	c.readMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.readMu.Unlock()
	c.doneOnce.Do(func() { close(c.done) })
}

func (c *Client) exitError() error {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readErr != nil {
		return c.readErr
	}
	return fmt.Errorf("MCP server exited")
}

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		done := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		stderr := strings.TrimSpace(c.stderr.String())
		if stderr != "" {
			c.logf("mcp[%s] stderr: %s", c.name, stderr)
		}
	})
}

type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.buf.Len() < b.max {
		remaining := b.max - b.buf.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.buf.Write(p)
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// StartAll starts every enabled server concurrently and returns the clients
// that connected successfully. Failures are reported through logf and do not
// abort startup.
func StartAll(ctx context.Context, servers map[string]config.MCPServer, logf func(string, ...any)) []*Client {
	if logf == nil {
		logf = defaultLogf
	}
	type result struct {
		name   string
		client *Client
	}
	results := make(chan result, len(servers))
	var wg sync.WaitGroup
	for name, srv := range servers {
		if srv.Disabled {
			continue
		}
		wg.Add(1)
		go func(name string, srv config.MCPServer) {
			defer wg.Done()
			timeout := time.Duration(srv.TimeoutSec) * time.Second
			client, err := Start(ctx, Server{
				Name:    name,
				Command: srv.Command,
				Args:    srv.Args,
				Env:     srv.Env,
				Timeout: timeout,
				Logf:    logf,
			})
			if err != nil {
				logf("mcp: %v", err)
				results <- result{name: name}
				return
			}
			results <- result{name: name, client: client}
		}(name, srv)
	}
	wg.Wait()
	close(results)

	byName := map[string]*Client{}
	for r := range results {
		if r.client != nil {
			byName[r.name] = r.client
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sortStrings(names)
	clients := make([]*Client, 0, len(names))
	for _, name := range names {
		clients = append(clients, byName[name])
	}
	return clients
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
