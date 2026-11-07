// Package trace persists agent event streams so runs can be inspected after
// the fact. Eval runs and web sessions share one file-backed store.
package trace

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"codedeep/internal/agent"
)

const (
	KindEval    = "eval"
	KindSession = "session"

	// DefaultMax bounds the store. Older traces are removed on save.
	DefaultMax = 500
)

// Meta is the cheap header of a trace. Listing decodes only this, so a store
// full of large event streams stays fast to browse.
type Meta struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Created     time.Time `json:"created"`
	DurationMS  int64     `json:"duration_ms,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Status      string    `json:"status,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Task        string    `json:"task,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Passed      *bool     `json:"passed,omitempty"`
	Failures    []string  `json:"failures,omitempty"`
	Warnings    []string  `json:"warnings,omitempty"`
	EventCount  int       `json:"event_count"`
	ModelCalls  int       `json:"model_calls,omitempty"`
	ToolCalls   int       `json:"tool_calls,omitempty"`
	TotalTokens int       `json:"total_tokens,omitempty"`
	CostUSD     float64   `json:"cost_usd,omitempty"`
}

// Verifier is an eval task's external verification result.
type Verifier struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output,omitempty"`
}

// Trace is one complete event stream plus its metadata.
type Trace struct {
	Meta
	Prompt   string        `json:"prompt,omitempty"`
	Verifier *Verifier     `json:"verifier,omitempty"`
	Events   []agent.Event `json:"events"`
}

// NewID builds a sortable, filename-safe identifier.
func NewID(kind, label string) string {
	stamp := time.Now().UTC().Format("20060102-150405")
	slug := slugify(label, 40)
	if slug == "" {
		slug = "trace"
	}
	return strings.Join([]string{stamp, slugify(kind, 12), slug, randomHex(3)}, "-")
}

func slugify(s string, limit int) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= limit {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(buf)
}

// validID rejects traversal and anything that is not a generated ID.
func validID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
