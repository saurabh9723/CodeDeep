package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"codedeep/internal/session"
	"codedeep/internal/tools"
)

const (
	maxTodos    = 50
	maxTodoText = 300
)

type todoItem struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

func (a *Agent) registerTodoTool() {
	if a.depth > 0 {
		return
	}
	a.reg.Register(&tools.Spec{
		Name: "todo_write",
		Description: "Replace the session's working plan with a todo list. Use it for multi-step tasks: " +
			"keep the list short, mark exactly one item in_progress while working, and mark items completed " +
			"as they finish. The plan is shown to the user and survives across turns.",
		Internal: true,
		Params: tools.Object(map[string]any{
			"todos": map[string]any{
				"type":        "array",
				"description": "The complete updated plan.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": tools.StrProp("What needs to be done."),
						"status": map[string]any{
							"type":        "string",
							"enum":        []string{"pending", "in_progress", "completed"},
							"description": "Progress state; defaults to pending.",
						},
					},
					"required": []string{"text"},
				},
			},
		}, "todos"),
		Run: a.runTodoWrite,
	})
}

func (a *Agent) runTodoWrite(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Todos []todoItem `json:"todos"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Todos) > maxTodos {
		return tools.Result{}, fmt.Errorf("the plan has %d items; the limit is %d", len(args.Todos), maxTodos)
	}
	todos := make([]session.Todo, 0, len(args.Todos))
	for i, item := range args.Todos {
		text := strings.TrimSpace(item.Text)
		if text == "" {
			return tools.Result{}, fmt.Errorf("item %d has empty text", i+1)
		}
		text = truncateRunes(text, maxTodoText)
		status, err := normalizeTodoStatus(item.Status)
		if err != nil {
			return tools.Result{}, fmt.Errorf("item %d: %w", i+1, err)
		}
		todos = append(todos, session.Todo{Text: text, Status: status})
	}

	a.locks.todos.Lock()
	sess := a.session
	if sess != nil {
		sess.Todos = todos
	}
	a.locks.todos.Unlock()
	if sess != nil {
		a.save(sess)
	}
	a.emitEvent(Event{Type: EventTodos, SessionID: sessionID(sess), Depth: a.depth, Todos: todos})
	return tools.Result{Output: formatTodos(todos)}, nil
}

func normalizeTodoStatus(status string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "pending", "todo", "open":
		return "pending", nil
	case "in_progress", "in-progress", "active", "doing":
		return "in_progress", nil
	case "completed", "complete", "done", "closed":
		return "completed", nil
	default:
		return "", fmt.Errorf("unknown status %q (use pending, in_progress, or completed)", status)
	}
}

func formatTodos(todos []session.Todo) string {
	done := 0
	for _, todo := range todos {
		if todo.Status == "completed" {
			done++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan updated: %d/%d complete\n", done, len(todos))
	for _, todo := range todos {
		marker := "[ ]"
		switch todo.Status {
		case "completed":
			marker = "[x]"
		case "in_progress":
			marker = "[~]"
		}
		fmt.Fprintf(&b, "%s %s\n", marker, todo.Text)
	}
	return strings.TrimSpace(b.String())
}

func truncateRunes(text string, max int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= max {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}
