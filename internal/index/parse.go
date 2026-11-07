package index

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"

	"codedeep/internal/util"
)

var languageByExt = map[string]string{
	".go":       "go",
	".py":       "python",
	".pyi":      "python",
	".js":       "javascript",
	".jsx":      "javascript",
	".mjs":      "javascript",
	".cjs":      "javascript",
	".ts":       "typescript",
	".tsx":      "typescript",
	".mts":      "typescript",
	".cts":      "typescript",
	".rs":       "rust",
	".java":     "java",
	".kt":       "kotlin",
	".kts":      "kotlin",
	".rb":       "ruby",
	".php":      "php",
	".c":        "c",
	".h":        "c",
	".cc":       "cpp",
	".cpp":      "cpp",
	".cxx":      "cpp",
	".hpp":      "cpp",
	".hh":       "cpp",
	".hxx":      "cpp",
	".cs":       "csharp",
	".swift":    "swift",
	".scala":    "scala",
	".sh":       "shell",
	".bash":     "shell",
	".zsh":      "shell",
	".md":       "markdown",
	".markdown": "markdown",
	".lua":      "lua",
	".ex":       "elixir",
	".exs":      "elixir",
}

func languageOf(rel string) string {
	return languageByExt[strings.ToLower(filepath.Ext(rel))]
}

func isBinary(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0
}

// parseSymbols extracts declarations for a supported language. Unknown
// languages return nil; files are still listed by the index.
func parseSymbols(rel, lang string, data []byte) []Symbol {
	switch lang {
	case "go":
		return goSymbols(rel, data)
	case "python":
		return pythonSymbols(rel, data)
	case "javascript", "typescript":
		return jsSymbols(rel, data)
	case "rust":
		return rustSymbols(rel, data)
	case "java", "csharp":
		return javaLikeSymbols(rel, data)
	case "kotlin":
		return kotlinSymbols(rel, data)
	case "ruby":
		return rubySymbols(rel, data)
	case "php":
		return phpSymbols(rel, data)
	case "c", "cpp":
		return cFamilySymbols(rel, data)
	case "swift":
		return swiftSymbols(rel, data)
	case "scala":
		return scalaSymbols(rel, data)
	case "shell":
		return shellSymbols(rel, data)
	case "markdown":
		return markdownSymbols(rel, data)
	case "lua":
		return luaSymbols(rel, data)
	case "elixir":
		return elixirSymbols(rel, data)
	}
	return nil
}

func goSymbols(rel string, data []byte) []Symbol {
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, rel, data, parser.SkipObjectResolution)
	if file == nil {
		return nil
	}
	var out []Symbol
	add := func(name, parent, kind, sig string, pos token.Pos, exported bool) {
		if name == "" || name == "_" {
			return
		}
		out = append(out, Symbol{
			Name:      name,
			Parent:    parent,
			Kind:      kind,
			Signature: shorten(sig, 200),
			File:      rel,
			Line:      fset.Position(pos).Line,
			Exported:  exported,
		})
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "func"
			parent := ""
			prefix := "func "
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				field := d.Recv.List[0]
				parent = receiverName(field.Type)
				recv := printNode(fset, field.Type)
				if len(field.Names) > 0 {
					recv = field.Names[0].Name + " " + recv
				}
				prefix += "(" + recv + ") "
			}
			sig := prefix + d.Name.Name + strings.TrimPrefix(printNode(fset, d.Type), "func")
			add(d.Name.Name, parent, kind, sig, d.Pos(), ast.IsExported(d.Name.Name))
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					kind := "type"
					sig := "type " + s.Name.Name
					switch s.Type.(type) {
					case *ast.StructType:
						kind, sig = "struct", sig+" struct"
					case *ast.InterfaceType:
						kind, sig = "interface", sig+" interface"
					}
					add(s.Name.Name, "", kind, sig, s.Pos(), ast.IsExported(s.Name.Name))
				case *ast.ValueSpec:
					kind := strings.ToLower(d.Tok.String())
					for _, name := range s.Names {
						add(name.Name, "", kind, kind+" "+name.Name, name.Pos(), ast.IsExported(name.Name))
					}
				}
			}
		}
	}
	return out
}

func printNode(fset *token.FileSet, node any) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, node); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

var (
	pyDefRe     = regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(([^)]*)\)`)
	pyClassRe   = regexp.MustCompile(`^class\s+([A-Za-z_]\w*)\s*(?:\(([^)]*)\))?`)
	pyDecorRe   = regexp.MustCompile(`^@`)
	pyCommentRe = regexp.MustCompile(`^#`)
)

func pythonSymbols(rel string, data []byte) []Symbol {
	type scope struct {
		indent int
		name   string
	}
	var out []Symbol
	var scopes []scope
	eachLine(data, func(lineNo int, line string) {
		if strings.TrimSpace(line) == "" || pyCommentRe.MatchString(strings.TrimSpace(line)) || pyDecorRe.MatchString(strings.TrimSpace(line)) {
			return
		}
		indent := indentWidth(line)
		for len(scopes) > 0 && indent <= scopes[len(scopes)-1].indent {
			scopes = scopes[:len(scopes)-1]
		}
		trimmed := strings.TrimSpace(line)
		if m := pyClassRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "class", "class "+m[1], rel, lineNo, !strings.HasPrefix(m[1], "_")))
			scopes = append(scopes, scope{indent: indent, name: m[1]})
			return
		}
		if m := pyDefRe.FindStringSubmatch(trimmed); m != nil {
			parent := ""
			if len(scopes) > 0 {
				parent = scopes[len(scopes)-1].name
			}
			kind := "func"
			if parent != "" {
				kind = "method"
			}
			out = append(out, newSymbol(m[1], parent, kind, "def "+m[1]+"("+m[2]+")", rel, lineNo, !strings.HasPrefix(m[1], "_")))
		}
	})
	return out
}

var jsPatterns = []struct {
	re    *regexp.Regexp
	kind  string
	build func(m []string) string
}{
	{regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*(\([^)]*\))`), "func",
		func(m []string) string { return "function " + m[1] + m[2] }},
	{regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class",
		func(m []string) string { return "class " + m[1] }},
	{regexp.MustCompile(`^(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`), "interface",
		func(m []string) string { return "interface " + m[1] }},
	{regexp.MustCompile(`^(?:export\s+)?type\s+([A-Za-z_$][\w$]*)\s*=`), "type",
		func(m []string) string { return "type " + m[1] }},
	{regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`), "func",
		func(m []string) string { return "const " + m[1] + " = " + m[2] + " =>" }},
}

func jsSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		for _, p := range jsPatterns {
			m := p.re.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			out = append(out, newSymbol(m[1], "", p.kind, p.build(m), rel, lineNo, true))
			return
		}
	})
	return out
}

var (
	rustFuncRe  = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+([A-Za-z_]\w*)\s*(?:<[^>]*>)?\s*\(([^)]*)\)`)
	rustTypeRe  = regexp.MustCompile(`^(?:pub\s+)?(struct|enum|trait|union|type|mod)\s+([A-Za-z_]\w*)`)
	rustValueRe = regexp.MustCompile(`^(?:pub\s+)?(const|static)\s+([A-Za-z_]\w*)`)
)

func rustSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := rustFuncRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "func", "fn "+m[1]+"("+m[2]+")", rel, lineNo, strings.HasPrefix(strings.TrimSpace(line), "pub")))
			return
		}
		if m := rustTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, strings.HasPrefix(trimmed, "pub")))
			return
		}
		if m := rustValueRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, strings.HasPrefix(trimmed, "pub")))
		}
	})
	return out
}

var (
	javaTypeRe = regexp.MustCompile(`^(?:(?:public|private|protected|static|final|abstract|sealed|non-sealed|partial)\s+)*(class|interface|enum|record|struct)\s+([A-Za-z_]\w*)`)
	javaFuncRe = regexp.MustCompile(`^(?:(?:public|private|protected|internal|static|final|abstract|synchronized|native|default|virtual|override|async|extern|unsafe|new|sealed)\s+)+[\w<>\[\],.?\s]+?\s+([A-Za-z_]\w*)\s*\(([^;{}]*)\)\s*(?:const\s*)?[;{]`)
)

func javaLikeSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := javaTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
			return
		}
		if m := javaFuncRe.FindStringSubmatch(trimmed); m != nil && !reservedNames[m[1]] {
			out = append(out, newSymbol(m[1], "", "method", m[1]+"("+shorten(m[2], 80)+")", rel, lineNo, true))
		}
	})
	return out
}

var (
	kotlinTypeRe = regexp.MustCompile(`^(?:(?:public|private|protected|internal|open|abstract|sealed|data|enum|annotation|value|inline|inner)\s+)*(class|interface|object)\s+([A-Za-z_]\w*)`)
	kotlinFuncRe = regexp.MustCompile(`^(?:(?:public|private|protected|internal|open|override|suspend|inline|operator|infix|tailrec|external|actual|expect|private)\s+)*fun\s+([A-Za-z_]\w*)\s*(?:<[^>]*>)?\s*\(([^)]*)\)`)
)

func kotlinSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := kotlinTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
			return
		}
		if m := kotlinFuncRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "func", "fun "+m[1]+"("+shorten(m[2], 80)+")", rel, lineNo, true))
		}
	})
	return out
}

var (
	rubyDefRe   = regexp.MustCompile(`^def\s+(?:self\.)?([A-Za-z_]\w*[?!=]?)`)
	rubyTypeRe  = regexp.MustCompile(`^(class|module)\s+([A-Za-z_]\w*(?:::[A-Za-z_]\w*)*)`)
	phpFuncRe   = regexp.MustCompile(`^(?:(?:public|private|protected|static|final|abstract)\s+)*function\s+([A-Za-z_]\w*)\s*\(([^)]*)\)`)
	phpTypeRe   = regexp.MustCompile(`^(?:(?:abstract|final)\s+)*(class|interface|trait|enum)\s+([A-Za-z_]\w*)`)
	swiftFuncRe = regexp.MustCompile(`^(?:(?:public|private|fileprivate|internal|open|static|class|final|override|mutating|nonmutating|convenience|required|dynamic)\s+)*func\s+([A-Za-z_]\w*)\s*(?:<[^>]*>)?\s*\(([^)]*)\)`)
	swiftTypeRe = regexp.MustCompile(`^(?:(?:public|private|fileprivate|internal|open|final)\s+)*(class|struct|enum|protocol|extension|actor)\s+([A-Za-z_]\w*)`)
)

func rubySymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := rubyDefRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "method", "def "+m[1], rel, lineNo, true))
			return
		}
		if m := rubyTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
		}
	})
	return out
}

func phpSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := phpTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
			return
		}
		if m := phpFuncRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "func", "function "+m[1]+"("+shorten(m[2], 80)+")", rel, lineNo, true))
		}
	})
	return out
}

func swiftSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := swiftTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
			return
		}
		if m := swiftFuncRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "func", "func "+m[1]+"("+shorten(m[2], 80)+")", rel, lineNo, true))
		}
	})
	return out
}

var (
	cTypeRe = regexp.MustCompile(`^(?:typedef\s+)?(class|struct|enum|union)\s+([A-Za-z_]\w*)`)
	cFuncRe = regexp.MustCompile(`^[A-Za-z_][\w:<>,*&\[\]\s]*?\s+([A-Za-z_]\w*)\s*\(([^;{}]*)\)\s*(?:const\s*)?[;{]`)
)

func cFamilySymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := cTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
			return
		}
		if m := cFuncRe.FindStringSubmatch(trimmed); m != nil && !reservedNames[m[1]] {
			out = append(out, newSymbol(m[1], "", "func", m[1]+"("+shorten(m[2], 80)+")", rel, lineNo, true))
		}
	})
	return out
}

var (
	scalaTypeRe = regexp.MustCompile(`^(?:(?:private|protected|final|sealed|abstract|implicit|case)\s+)*(class|object|trait|enum)\s+([A-Za-z_]\w*)`)
	scalaDefRe  = regexp.MustCompile(`^(?:(?:private|protected|final|override|implicit|inline)\s+)*def\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*\(([^)]*)\)`)
)

func scalaSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if m := scalaTypeRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[2], "", m[1], m[1]+" "+m[2], rel, lineNo, true))
			return
		}
		if m := scalaDefRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "def", "def "+m[1]+"("+shorten(m[2], 80)+")", rel, lineNo, true))
		}
	})
	return out
}

var shellFuncRe = regexp.MustCompile(`^(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\s*\)\s*\{`)

func shellSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			return
		}
		if m := shellFuncRe.FindStringSubmatch(trimmed); m != nil {
			out = append(out, newSymbol(m[1], "", "func", m[1]+"()", rel, lineNo, true))
		}
	})
	return out
}

var mdHeadingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

func markdownSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	inFence := false
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			return
		}
		if inFence {
			return
		}
		if m := mdHeadingRe.FindStringSubmatch(trimmed); m != nil {
			name := strings.TrimSpace(m[2])
			out = append(out, newSymbol(name, "", "heading", m[1]+" "+name, rel, lineNo, true))
		}
	})
	return out
}

var (
	luaFuncRe  = regexp.MustCompile(`^(?:local\s+)?function\s+([A-Za-z_][\w.:]*)`)
	elixirRe   = regexp.MustCompile(`^(defmodule|defmacro|defp|def|defstruct)\s+([A-Za-z_][\w.!?]*)`)
	elixirKind = map[string]string{"defmodule": "module", "defstruct": "struct", "def": "func", "defp": "func", "defmacro": "macro"}
)

func luaSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			return
		}
		if m := luaFuncRe.FindStringSubmatch(trimmed); m != nil {
			name := m[1]
			parent := ""
			if idx := strings.LastIndex(name, "."); idx > 0 {
				parent, name = name[:idx], name[idx+1:]
			}
			out = append(out, newSymbol(name, parent, "func", "function "+m[1], rel, lineNo, true))
		}
	})
	return out
}

func elixirSymbols(rel string, data []byte) []Symbol {
	var out []Symbol
	eachLine(data, func(lineNo int, line string) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			return
		}
		if m := elixirRe.FindStringSubmatch(trimmed); m != nil {
			kind := elixirKind[m[1]]
			out = append(out, newSymbol(m[2], "", kind, m[1]+" "+m[2], rel, lineNo, true))
		}
	})
	return out
}

var reservedNames = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"sizeof": true, "catch": true, "else": true, "do": true, "new": true,
	"delete": true, "throw": true, "case": true, "using": true, "namespace": true,
	"typedef": true, "template": true, "operator": true, "foreach": true, "lock": true,
	"typeof": true, "await": true, "yield": true, "raise": true, "with": true,
}

func newSymbol(name, parent, kind, sig, rel string, line int, exported bool) Symbol {
	return Symbol{
		Name:      name,
		Parent:    parent,
		Kind:      kind,
		Signature: shorten(sig, 200),
		File:      rel,
		Line:      line,
		Exported:  exported,
	}
}

func eachLine(data []byte, fn func(lineNo int, line string)) {
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		line := strings.TrimSuffix(string(raw), "\r")
		fn(i+1, line)
	}
}

func indentWidth(line string) int {
	width := 0
	for _, r := range line {
		switch r {
		case ' ':
			width++
		case '\t':
			width += 4
		default:
			return width
		}
	}
	return width
}

func shorten(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return util.Truncate(s, max)
}
