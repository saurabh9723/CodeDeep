package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"codedeep/internal/llm"
)

type Result struct {
	Output  string
	IsError bool
}

type Spec struct {
	Name        string
	Description string
	Params      json.RawMessage
	ReadOnly    bool
	Run         func(ctx context.Context, args json.RawMessage) (Result, error)
}

type Registry struct {
	mu    sync.RWMutex
	order []string
	specs map[string]*Spec
}

func NewRegistry() *Registry {
	return &Registry{specs: map[string]*Spec{}}
}

func (r *Registry) Register(s *Spec) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.specs[s.Name]; !exists {
		r.order = append(r.order, s.Name)
	}
	r.specs[s.Name] = s
}

func (r *Registry) Get(name string) (*Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[name]
	return s, ok
}

func (r *Registry) List() []*Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Spec, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.specs[name])
	}
	return out
}

func (r *Registry) Definitions() []llm.Tool {
	specs := r.List()
	defs := make([]llm.Tool, 0, len(specs))
	for _, s := range specs {
		defs = append(defs, llm.Tool{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.Params,
			},
		})
	}
	return defs
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}

func Object(props map[string]any, required ...string) json.RawMessage {
	obj := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		obj["required"] = required
	}
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	return b
}

func StrProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func BoolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func IntProp(desc string, def int) map[string]any {
	m := map[string]any{"type": "integer", "description": desc}
	if def > 0 {
		m["default"] = def
	}
	return m
}
