package index

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"

	"codedeep/internal/util"
)

type fileView struct {
	meta    File
	symbols []Symbol
}

// MapOptions controls the rendered repository map.
type MapOptions struct {
	Query          string
	Path           string
	MaxTokens      int
	MaxFiles       int
	SymbolsPerFile int
}

// MapResult is a rendered repository map plus the counts behind it.
type MapResult struct {
	Text         string `json:"text"`
	Files        int    `json:"files"`
	Symbols      int    `json:"symbols"`
	ShownFiles   int    `json:"shown_files"`
	ShownSymbols int    `json:"shown_symbols"`
	Tokens       int    `json:"tokens"`
	Truncated    bool   `json:"truncated"`
}

func renderMap(views []fileView, opts MapOptions) MapResult {
	budget := opts.MaxTokens
	if budget <= 0 {
		budget = 1200
	}
	if budget < 200 {
		budget = 200
	}
	if budget > 8000 {
		budget = 8000
	}
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 200
	}
	if maxFiles > 1000 {
		maxFiles = 1000
	}
	perFile := opts.SymbolsPerFile
	if perFile <= 0 {
		perFile = 10
	}
	if perFile > 40 {
		perFile = 40
	}

	scope := filterPath(views, opts.Path)
	query := strings.TrimSpace(opts.Query)
	terms := queryTerms(query)
	ranked := rankFiles(scope, terms)

	totalSymbols := 0
	for _, view := range scope {
		totalSymbols += len(view.symbols)
	}

	header := fmt.Sprintf("repo map: %d files, %d symbols", len(scope), totalSymbols)
	if query != "" {
		header += fmt.Sprintf(" matching %q", util.Truncate(query, 80))
	}
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	used := estimateTokens(header) + 1

	res := MapResult{Files: len(scope), Symbols: totalSymbols}
	for _, view := range ranked {
		if res.ShownFiles >= maxFiles {
			res.Truncated = true
			break
		}
		line := view.meta.Path
		if view.meta.Lines > 0 {
			line += fmt.Sprintf(" (%d lines)", view.meta.Lines)
		}
		cost := estimateTokens(line) + 1
		if used+cost > budget {
			res.Truncated = true
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
		used += cost
		res.ShownFiles++

		for _, sym := range pickSymbols(view.symbols, terms, perFile) {
			text := "  "
			if sym.Signature != "" {
				text += util.Truncate(sym.Signature, 110)
			} else {
				text += sym.Name
			}
			text += fmt.Sprintf(" :%d\n", sym.Line)
			cost := estimateTokens(text) + 1
			if used+cost > budget {
				res.Truncated = true
				break
			}
			b.WriteString(text)
			used += cost
			res.ShownSymbols++
		}
		if res.Truncated {
			break
		}
	}
	if res.Truncated {
		fmt.Fprintf(&b, "[truncated at ~%d tokens; refine the query or raise the budget]\n", budget)
	}
	res.Text = b.String()
	res.Tokens = estimateTokens(res.Text)
	return res
}

func renderPrompt(views []fileView, query string, maxTokens int) string {
	const intro = "# Codebase map\n" +
		"Ranked files and declarations for the current request. Use find_symbol for exact lookups; read files before editing.\n\n"
	introTokens := estimateTokens(intro)
	budget := maxTokens - introTokens
	if budget < 200 {
		budget = 200
	}
	res := renderMap(views, MapOptions{Query: query, MaxTokens: budget})
	if res.ShownFiles == 0 && res.Symbols == 0 {
		return ""
	}
	return intro + res.Text
}

func filterPath(views []fileView, prefix string) []fileView {
	prefix = strings.TrimSpace(strings.ReplaceAll(prefix, "\\", "/"))
	prefix = strings.TrimPrefix(prefix, "./")
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return views
	}
	prefix += "/"
	out := make([]fileView, 0, len(views))
	for _, view := range views {
		p := view.meta.Path
		if strings.HasPrefix(p, prefix) || p == strings.TrimSuffix(prefix, "/") {
			out = append(out, view)
		}
	}
	return out
}

func rankFiles(views []fileView, terms []string) []fileView {
	type scored struct {
		view  fileView
		score int
	}
	ranked := make([]scored, 0, len(views))
	for _, view := range views {
		score := fileScore(view, terms)
		if len(terms) > 0 && score == 0 {
			continue
		}
		ranked = append(ranked, scored{view: view, score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if len(ranked[i].view.symbols) != len(ranked[j].view.symbols) {
			return len(ranked[i].view.symbols) > len(ranked[j].view.symbols)
		}
		return ranked[i].view.meta.Path < ranked[j].view.meta.Path
	})
	out := make([]fileView, len(ranked))
	for i, item := range ranked {
		out[i] = item.view
	}
	return out
}

func fileScore(view fileView, terms []string) int {
	meta, syms := view.meta, view.symbols
	base := strings.ToLower(path.Base(meta.Path))
	if len(terms) == 0 {
		score := len(syms)
		if score > 12 {
			score = 12
		}
		switch strings.Count(meta.Path, "/") {
		case 0:
			score += 4
		case 1:
			score += 2
		}
		switch {
		case strings.HasPrefix(base, "readme"):
			score += 6
		case base == "go.mod", base == "package.json", base == "cargo.toml",
			base == "pyproject.toml", base == "makefile", base == "dockerfile":
			score += 4
		case base == "main.go", base == "main.rs", base == "main.py",
			base == "index.js", base == "index.ts", base == "__init__.py",
			base == "lib.rs", base == "mod.rs":
			score += 3
		}
		return score
	}

	pathLower := strings.ToLower(meta.Path)
	score := 0
	for _, term := range terms {
		switch {
		case base == term:
			score += 25
		case strings.HasPrefix(base, term):
			score += 12
		case strings.Contains(base, term):
			score += 8
		}
		if strings.Contains(pathLower, term) {
			score += 6
		}
		for _, sym := range syms {
			name := strings.ToLower(sym.Name)
			qual := strings.ToLower(sym.Qualified())
			switch {
			case name == term:
				score += 20
			case strings.HasPrefix(name, term):
				score += 12
			case strings.Contains(name, term):
				score += 8
			case strings.Contains(qual, term):
				score += 6
			case strings.Contains(strings.ToLower(sym.Signature), term):
				score += 3
			}
		}
	}
	if score > 0 {
		score += len(syms)
		if score > 40 {
			score = 40
		}
	}
	return score
}

func pickSymbols(syms []Symbol, terms []string, limit int) []Symbol {
	if len(syms) <= limit && len(terms) == 0 {
		return syms
	}
	if len(terms) == 0 {
		return syms[:limit]
	}
	type scored struct {
		sym   Symbol
		score int
	}
	ranked := make([]scored, 0, len(syms))
	for _, sym := range syms {
		name := strings.ToLower(sym.Name)
		qual := strings.ToLower(sym.Qualified())
		score := 0
		for _, term := range terms {
			switch {
			case name == term:
				score += 20
			case strings.HasPrefix(name, term):
				score += 12
			case strings.Contains(name, term):
				score += 8
			case strings.Contains(qual, term):
				score += 6
			case strings.Contains(strings.ToLower(sym.Signature), term):
				score += 3
			}
		}
		if score > 0 {
			ranked = append(ranked, scored{sym: sym, score: score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].sym.Line < ranked[j].sym.Line
	})
	out := make([]Symbol, 0, limit)
	for _, item := range ranked {
		out = append(out, item.sym)
		if len(out) >= limit {
			return out
		}
	}
	// Fill the remaining slots with declarations that still add context.
	for _, sym := range syms {
		if len(out) >= limit {
			break
		}
		dup := false
		for _, shown := range out {
			if shown.Line == sym.Line && shown.File == sym.File {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, sym)
		}
	}
	return out
}

func searchSymbols(symbols []Symbol, query, pathPrefix, kind string, limit int) []Symbol {
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	kind = normalizeKind(kind)
	prefix := strings.Trim(strings.TrimSpace(strings.ReplaceAll(pathPrefix, "\\", "/")), "/")
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	terms := queryTerms(lowerQuery)
	if len(terms) == 0 {
		terms = []string{lowerQuery}
	}

	type scored struct {
		sym   Symbol
		score int
	}
	ranked := make([]scored, 0, limit*2)
	seen := map[string]bool{}
	for _, sym := range symbols {
		if kind != "" && strings.ToLower(sym.Kind) != kind {
			continue
		}
		if prefix != "" && !strings.HasPrefix(sym.File, prefix+"/") {
			continue
		}
		name := strings.ToLower(sym.Name)
		qual := strings.ToLower(sym.Qualified())
		score := 0
		if lowerQuery != "" {
			switch {
			case qual == lowerQuery:
				score += 130
			case name == lowerQuery:
				score += 120
			case strings.HasPrefix(qual, lowerQuery):
				score += 80
			case strings.HasPrefix(name, lowerQuery):
				score += 90
			case strings.Contains(qual, lowerQuery):
				score += 55
			case strings.Contains(name, lowerQuery):
				score += 65
			}
			matched := 0
			for _, term := range terms {
				ts := termScore(term, name, qual)
				if ts > 0 {
					matched++
				} else if !strings.Contains(strings.ToLower(sym.File), term) {
					continue
				}
				score += ts
			}
			if score == 0 && subsequence(strings.ReplaceAll(lowerQuery, " ", ""), qual) {
				score = 40
			}
			if score == 0 {
				continue
			}
			if matched == len(terms) && len(terms) > 1 {
				score += 10
			}
		}
		if sym.Exported {
			score += 2
		}
		key := fmt.Sprintf("%s|%d|%s|%s", sym.File, sym.Line, sym.Kind, sym.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		ranked = append(ranked, scored{sym: sym, score: score})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].sym.Exported != ranked[j].sym.Exported {
			return ranked[i].sym.Exported
		}
		if len(ranked[i].sym.Name) != len(ranked[j].sym.Name) {
			return len(ranked[i].sym.Name) < len(ranked[j].sym.Name)
		}
		if ranked[i].sym.File != ranked[j].sym.File {
			return ranked[i].sym.File < ranked[j].sym.File
		}
		return ranked[i].sym.Line < ranked[j].sym.Line
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Symbol, len(ranked))
	for i, item := range ranked {
		out[i] = item.sym
	}
	return out
}

func termScore(term, name, qual string) int {
	switch {
	case name == term:
		return 30
	case qual == term:
		return 28
	case strings.HasPrefix(name, term):
		return 18
	case strings.HasPrefix(qual, term):
		return 16
	case strings.Contains(name, term):
		return 12
	case strings.Contains(qual, term):
		return 10
	case len(term) >= 4 && subsequence(term, name):
		return 6
	}
	return 0
}

func normalizeKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "", "all":
		return ""
	case "function", "fn":
		return "func"
	case "struct", "interface", "class", "enum", "type", "func", "method",
		"const", "var", "heading", "module", "macro", "trait", "def":
		return kind
	}
	return kind
}

func queryTerms(query string) []string {
	query = strings.ToLower(strings.TrimSpace(query))
	var terms []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 2 {
			terms = append(terms, cur.String())
		}
		cur.Reset()
	}
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	if len(terms) == 0 && query != "" {
		terms = []string{query}
	}
	return terms
}

func subsequence(needle, hay string) bool {
	if needle == "" {
		return false
	}
	j := 0
	for i := 0; i < len(hay) && j < len(needle); i++ {
		if hay[i] == needle[j] {
			j++
		}
	}
	return j == len(needle)
}

func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}
