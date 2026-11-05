package tools

import (
	"context"
	"encoding/json"
)

// RegisterCore wires the built-in coding tools into a registry.
func RegisterCore(r *Registry, f *FS, shell string) {
	r.Register(&Spec{
		Name: "read_file",
		Description: "Read a text file from the workspace. Returns 1-based numbered lines. " +
			"Read a file before editing it. For large files use offset/limit (default 400 lines, max 2000).",
		ReadOnly: true,
		Params: Object(map[string]any{
			"path":   StrProp("Workspace-relative or absolute path to the file."),
			"offset": IntProp("1-based line number to start reading from.", 1),
			"limit":  IntProp("Maximum number of lines to return.", 400),
		}, "path"),
		Run: f.readFile,
	})

	r.Register(&Spec{
		Name: "write_file",
		Description: "Create or overwrite a file with the given full content. Parent directories are created. " +
			"Prefer edit_file for small changes to an existing file; use this for new files or full rewrites.",
		Params: Object(map[string]any{
			"path":    StrProp("Workspace-relative or absolute path to write."),
			"content": StrProp("Complete file content."),
		}, "path", "content"),
		Run: f.writeFile,
	})

	r.Register(&Spec{
		Name: "edit_file",
		Description: "Replace an exact substring in an existing file. old_string must match the file exactly, " +
			"including whitespace; include enough surrounding context to make it unique. " +
			"Set replace_all=true to replace every occurrence.",
		Params: Object(map[string]any{
			"path":        StrProp("Path of the file to edit."),
			"old_string":  StrProp("Exact text to find."),
			"new_string":  StrProp("Replacement text."),
			"replace_all": BoolProp("Replace all occurrences instead of requiring a unique match."),
		}, "path", "old_string", "new_string"),
		Run: f.editFile,
	})

	r.Register(&Spec{
		Name: "glob",
		Description: "Find files by glob pattern. Supports *, ?, [...] and ** (recursive). " +
			"Examples: '**/*.go', 'src/**/*.ts'. Returns workspace-relative paths, sorted.",
		ReadOnly: true,
		Params: Object(map[string]any{
			"pattern":     StrProp("Glob pattern relative to the search path."),
			"path":        StrProp("Directory to search in. Defaults to the workspace root."),
			"max_results": IntProp("Maximum number of paths to return (max 1000).", 200),
		}, "pattern"),
		Run: f.globFiles,
	})

	r.Register(&Spec{
		Name: "grep",
		Description: "Search file contents with a Go/RE2 regular expression. Returns path:line:text. " +
			"Use glob to restrict file types (e.g. '*.go') and ignore_case for case-insensitive search. " +
			"Skips binaries, .git, node_modules, vendor, build directories and files over 5 MB.",
		ReadOnly: true,
		Params: Object(map[string]any{
			"pattern":     StrProp("Regular expression (RE2) to search for."),
			"path":        StrProp("File or directory to search. Defaults to the workspace root."),
			"glob":        StrProp("Optional file filter, e.g. '*.go' or 'src/**/*.ts'."),
			"ignore_case": BoolProp("Case-insensitive matching."),
			"max_results": IntProp("Maximum number of matches to return (max 1000).", 100),
		}, "pattern"),
		Run: f.grepFiles,
	})

	r.Register(&Spec{
		Name: "bash",
		Description: "Run a shell command in the workspace. Returns combined stdout/stderr and the exit code. " +
			"Use for builds, tests, package managers, git, and other non-interactive commands. " +
			"Set timeout_sec for long-running commands (default 120, max 900). Do not run interactive programs.",
		Params: Object(map[string]any{
			"command":     StrProp("Shell command to execute."),
			"timeout_sec": IntProp("Timeout in seconds.", 120),
		}, "command"),
		Run: func(ctx context.Context, raw json.RawMessage) (Result, error) {
			return f.runShell(ctx, shell, raw)
		},
	})
}
