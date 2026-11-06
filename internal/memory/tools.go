package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"codedeep/internal/tools"
	"codedeep/internal/util"
)

const (
	maxRecallBody  = 4000
	maxRecallTotal = 16000
)

// RegisterTools adds remember, recall, and forget to a tool registry.
func RegisterTools(r *tools.Registry, store *Store) {
	if r == nil || store == nil {
		return
	}
	r.Register(&tools.Spec{
		Name: "remember",
		Description: "Save a durable note that will be available in future sessions. Use it for project knowledge " +
			"worth keeping: build and test commands, architecture decisions, conventions, and gotchas. " +
			"Do not store secrets, code dumps, or transient task state. Saving over an existing name replaces it.",
		Internal: true,
		Params: tools.Object(map[string]any{
			"name":        tools.StrProp("Short kebab-case identifier, e.g. 'build-command'."),
			"description": tools.StrProp("One line describing when the note applies."),
			"body":        tools.StrProp("The note itself, as concise markdown."),
			"scope":       scopeProp(),
		}, "name", "body"),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var args struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Body        string `json:"body"`
				Scope       string `json:"scope"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
			}
			note, err := store.Save(args.Scope, args.Name, args.Description, args.Body)
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Output: fmt.Sprintf("Saved memory %q (%s) at %s", note.Name, note.Scope, note.Path)}, nil
		},
	})

	r.Register(&tools.Spec{
		Name: "recall",
		Description: "Read durable notes saved with remember. With no query it lists the memory index; " +
			"with a query it returns the matching notes in full. Check memory before re-investigating a " +
			"convention or command that may already be recorded.",
		ReadOnly: true,
		Params: tools.Object(map[string]any{
			"query": tools.StrProp("Words to search for. Leave empty to list the index."),
		}),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
			}
			notes := store.Search(args.Query)
			if len(notes) == 0 {
				if strings.TrimSpace(args.Query) == "" {
					return tools.Result{Output: "No project memory yet. Use remember to save durable notes."}, nil
				}
				return tools.Result{Output: fmt.Sprintf("No memory matched %q.", args.Query)}, nil
			}
			var b strings.Builder
			if strings.TrimSpace(args.Query) == "" {
				fmt.Fprintf(&b, "%d memory note(s):\n", len(notes))
				for _, note := range notes {
					fmt.Fprintf(&b, "- %s [%s] — %s\n", note.Name, note.Scope, note.Description)
				}
				return tools.Result{Output: strings.TrimSpace(b.String())}, nil
			}
			fmt.Fprintf(&b, "%d matching memory note(s):\n", len(notes))
			for _, note := range notes {
				if b.Len() >= maxRecallTotal {
					b.WriteString("\n[more matches omitted]\n")
					break
				}
				fmt.Fprintf(&b, "\n## %s [%s]\n%s\n\n%s\n", note.Name, note.Scope, note.Description,
					util.TruncateMiddle(note.Body, maxRecallBody))
			}
			return tools.Result{Output: strings.TrimSpace(b.String())}, nil
		},
	})

	r.Register(&tools.Spec{
		Name: "forget",
		Description: "Delete a durable memory note that is wrong or obsolete. Give the note name; " +
			"the project scope is removed first, then global.",
		Internal: true,
		Params: tools.Object(map[string]any{
			"name":  tools.StrProp("Name of the note to delete."),
			"scope": scopeProp(),
		}, "name"),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var args struct {
				Name  string `json:"name"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return tools.Result{}, fmt.Errorf("invalid arguments: %w", err)
			}
			if strings.TrimSpace(args.Scope) == "" {
				if note, err := store.Delete("project", args.Name); err == nil {
					return tools.Result{Output: fmt.Sprintf("Deleted memory %q (%s) at %s", note.Name, note.Scope, note.Path)}, nil
				}
				note, err := store.Delete("global", args.Name)
				if err != nil {
					return tools.Result{}, fmt.Errorf("memory %q not found", args.Name)
				}
				return tools.Result{Output: fmt.Sprintf("Deleted memory %q (%s) at %s", note.Name, note.Scope, note.Path)}, nil
			}
			note, err := store.Delete(args.Scope, args.Name)
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Output: fmt.Sprintf("Deleted memory %q (%s) at %s", note.Name, note.Scope, note.Path)}, nil
		},
	})
}

func scopeProp() map[string]any {
	return map[string]any{
		"type":        "string",
		"enum":        []string{"project", "global"},
		"description": "project (default) is shared by this workspace; global is shared by every workspace.",
	}
}
