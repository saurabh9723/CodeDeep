package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"codedeep/internal/tools"
)

var unsafeToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func sanitize(s string) string {
	s = unsafeToolChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "unnamed"
	}
	return s
}

// Register adds every tool exposed by the server to the registry. MCP tools
// are asked for approval unless the server marks them read-only.
func (c *Client) Register(ctx context.Context, reg *tools.Registry, logf func(string, ...any)) int {
	if logf == nil {
		logf = defaultLogf
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	list, err := c.ListTools(ctx)
	if err != nil {
		logf("mcp[%s]: list tools: %v", c.name, err)
		return 0
	}
	serverName := sanitize(c.name)
	for _, t := range list {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		toolName := "mcp__" + serverName + "__" + sanitize(t.Name)
		original := t.Name
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		readOnly := t.Annotations != nil && t.Annotations.ReadOnlyHint
		desc := t.Description
		if desc == "" {
			desc = "MCP tool " + original
		}
		desc = fmt.Sprintf("%s [MCP server: %s]", desc, c.name)
		reg.Register(&tools.Spec{
			Name:        toolName,
			Description: desc,
			Params:      schema,
			ReadOnly:    readOnly,
			Run: func(ctx context.Context, args json.RawMessage) (tools.Result, error) {
				callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
				defer cancel()
				res, err := c.CallTool(callCtx, original, args)
				if err != nil {
					return tools.Result{}, err
				}
				return tools.Result{Output: res.Output, IsError: res.IsError}, nil
			},
		})
	}
	logf("mcp[%s]: registered %d tool(s)", c.name, len(list))
	return len(list)
}
