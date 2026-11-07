package util

// ignoredDirs are directories that never contain project source worth walking:
// dependency caches, build output, and tool metadata.
var ignoredDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "bower_components": true, "vendor": true,
	"dist": true, "build": true, "target": true, "out": true,
	".next": true, ".nuxt": true, ".svelte-kit": true,
	".cache": true, "coverage": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	".venv": true, "venv": true, "env": true,
	".idea": true, ".gradle": true,
}

// IgnoredDir reports whether a directory name should be skipped by workspace
// walks. Callers compare the base name, not the full path.
func IgnoredDir(name string) bool {
	return ignoredDirs[name]
}
