// Command mcp_echo is a minimal MCP stdio server used by the eval suite.
// It implements initialize, tools/list, and tools/call over newline-delimited
// JSON-RPC 2.0.
package main

import (
	"bufio"
	"encoding/json"
	"os"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		data, _ := json.Marshal(handle(req))
		out.Write(data)
		out.WriteByte('\n')
		out.Flush()
	}
}

func handle(req request) response {
	resp := response{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "eval-echo", "version": "1.0.0"},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": []any{map[string]any{
			"name":        "echo",
			"description": "Echo the provided text back to the caller.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "Text to echo"},
				},
				"required": []string{"text"},
			},
		}}}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			return resp
		}
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "echo: " + args.Text}},
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	return resp
}