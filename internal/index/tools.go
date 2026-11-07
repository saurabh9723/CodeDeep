package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"codedeep/internal/tools"
)

// RegisterTools exposes repo_map and find_symbol to the model.
func RegisterTools(r *tools.Registry, idx *Index) {
	r.Register(&tools.Spec{
		Name: "repo_map",
		Description: "Show a ranked map of the workspace: files with their key declarations and line numbers, " +
			"budgeted by tokens. Use it to orient yourself before searching or when you need to find where a " +
			"concept lives. Pass query to rank files and symbols related to a task. Prefer find_symbol when " +
			"you already know the declaration name.",
		ReadOnly: true,
		Params: tools.Object(map[string]any{
			"query":      tools.StrProp("Optional text, identifier, or task description used to rank files and symbols."),
			"path":       tools.StrProp("Optional directory prefix to limit the map to, e.g. 'internal/web'."),
			"max_tokens": tools.IntProp("Approximate token budget for the result (default 1200, max 8000).", 1200),
		}),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			return runRepoMap(ctx, idx, raw)
		},
	})

	r.Register(&tools.Spec{
		Name: "find_symbol",
		Description: "Find function, method, type, class, and constant declarations by name across the workspace. " +
			"Matches exact names, prefixes, substrings, and fuzzy spellings, and returns path:line with the " +
			"declaration signature. Use it instead of grepping for a known identifier.",
		ReadOnly: true,
		Params: tools.Object(map[string]any{
			"name":        tools.StrProp("Symbol name or fragment, e.g. 'Resolve', 'Registry.Get', or 'newfs'."),
			"kind":        tools.StrProp("Optional declaration kind: func, method, type, struct, interface, class, const, var, heading."),
			"path":        tools.StrProp("Optional directory prefix to search under."),
			"max_results": tools.IntProp("Maximum declarations to return (default 20, max 100).", 20),
		}, "name"),
		Run: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			return runFindSymbol(ctx, idx, raw)
		},
	})
}

type repoMapArgs struct {
	Query     string `json:"query"`
	Path      string `json:"path"`
	MaxTokens int    `json:"max_tokens"`
}

func runRepoMap(ctx context.Context, idx *Index, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	var args repoMapArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("repo_map: invalid arguments: %w", err)
	}
	res, err := idx.Map(MapOptions{
		Query:     args.Query,
		Path:      args.Path,
		MaxTokens: args.MaxTokens,
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("repo_map: %w", err)
	}
	if res.ShownFiles == 0 {
		return tools.Result{Output: "The workspace index has no matching files."}, nil
	}
	return tools.Result{Output: res.Text}, nil
}

type findSymbolArgs struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

func runFindSymbol(ctx context.Context, idx *Index, raw json.RawMessage) (tools.Result, error) {
	if err := ctx.Err(); err != nil {
		return tools.Result{}, err
	}
	var args findSymbolArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Result{}, fmt.Errorf("find_symbol: invalid arguments: %w", err)
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return tools.Result{}, errors.New("find_symbol: name is required")
	}
	limit := args.MaxResults
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	syms, err := idx.SearchSymbols(name, args.Path, args.Kind, limit)
	if err != nil {
		return tools.Result{}, fmt.Errorf("find_symbol: %w", err)
	}
	if len(syms) == 0 {
		return tools.Result{Output: fmt.Sprintf("No declarations matched %q. Try a shorter name or use grep for text matches.", name)}, nil
	}
	var b strings.Builder
	for _, sym := range syms {
		text := sym.Signature
		if text == "" {
			text = sym.Kind + " " + sym.Name
		}
		fmt.Fprintf(&b, "%s:%d  %s\n", sym.File, sym.Line, text)
	}
	if len(syms) >= limit {
		fmt.Fprintf(&b, "[showing %d results; refine name, kind, or path]\n", limit)
	}
	return tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
}
