package agent

import (
	"encoding/json"

	"codedeep/internal/diagnostics"
	"codedeep/internal/llm"
	"codedeep/internal/session"
)

const (
	EventUser           = "user"
	EventAssistantStart = "assistant_start"
	EventText           = "text"
	EventReasoning      = "reasoning"
	EventToolStart      = "tool_start"
	EventToolEnd        = "tool_end"
	EventDiff           = "diff"
	EventApproval       = "approval_request"
	EventApprovalResult = "approval_result"
	EventUsage          = "usage"
	EventCompact        = "compact"
	EventSubagentStart  = "subagent_start"
	EventSubagentEnd    = "subagent_end"
	EventHook           = "hook"
	EventDiagnostics    = "diagnostics"
	EventTodos          = "todos"
	EventVerifyStart    = "verify_start"
	EventVerify         = "verify"
	EventRetry          = "retry"
	EventSaved          = "saved"
	EventDone           = "done"
	EventError          = "error"
)

// Event is the single source of truth for CLI and web rendering.
type Event struct {
	Type        string                   `json:"type"`
	SessionID   string                   `json:"session_id,omitempty"`
	Text        string                   `json:"text,omitempty"`
	Images      []llm.Image              `json:"images,omitempty"`
	Name        string                   `json:"name,omitempty"`
	Args        json.RawMessage          `json:"args,omitempty"`
	Output      string                   `json:"output,omitempty"`
	Diff        string                   `json:"diff,omitempty"`
	Path        string                   `json:"path,omitempty"`
	IsError     bool                     `json:"is_error,omitempty"`
	ApprovalID  string                   `json:"approval_id,omitempty"`
	ReadOnly    bool                     `json:"read_only,omitempty"`
	Depth       int                      `json:"depth,omitempty"`
	CallID      string                   `json:"call_id,omitempty"`
	Usage       *llm.Usage               `json:"usage,omitempty"`
	TotalUsage  *llm.Usage               `json:"total_usage,omitempty"`
	ToolUsage   map[string]llm.ToolUsage `json:"tool_usage,omitempty"`
	CostUSD     float64                  `json:"cost_usd,omitempty"`
	TotalCost   float64                  `json:"total_cost_usd,omitempty"`
	Checkpoints int                      `json:"checkpoints,omitempty"`
	Retry       *llm.RetryNotice         `json:"retry,omitempty"`
	Message     string                   `json:"message,omitempty"`
	Annotations []diagnostics.Diagnostic `json:"annotations,omitempty"`
	Todos       []session.Todo           `json:"todos,omitempty"`
	// Seq is assigned by the web transport so reconnecting clients can resume
	// exactly where they left off. Other emitters leave it zero.
	Seq int64 `json:"seq,omitempty"`
}

type Emitter func(Event)

func (e Emitter) Emit(ev Event) {
	if e != nil {
		e(ev)
	}
}
