// Package index builds a lightweight symbol map of the workspace. It is used
// by the repo_map and find_symbol tools, by the system prompt, and by the web
// file palette. The index is model-agnostic: it only depends on file contents.
package index

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codedeep/internal/util"
)

// File describes one indexed workspace file.
type File struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`
	Lines    int    `json:"lines,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Symbols  int    `json:"symbols"`
}

// Symbol is one declaration found in a file.
type Symbol struct {
	Name      string `json:"name"`
	Parent    string `json:"parent,omitempty"`
	Kind      string `json:"kind"`
	Signature string `json:"signature,omitempty"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Exported  bool   `json:"exported,omitempty"`
}

// Qualified returns the parent-scoped name, such as "Registry.Get".
func (s Symbol) Qualified() string {
	if s.Parent == "" {
		return s.Name
	}
	return s.Parent + "." + s.Name
}

// Options controls how much of the workspace is indexed.
type Options struct {
	MaxFiles        int
	MaxFileBytes    int64
	RefreshInterval time.Duration
}

func DefaultOptions() Options {
	return Options{
		MaxFiles:        5000,
		MaxFileBytes:    1 << 20,
		RefreshInterval: 2 * time.Second,
	}
}

func (o Options) normalized() Options {
	def := DefaultOptions()
	if o.MaxFiles <= 0 {
		o.MaxFiles = def.MaxFiles
	}
	if o.MaxFiles > 50000 {
		o.MaxFiles = 50000
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = def.MaxFileBytes
	}
	if o.RefreshInterval < 0 {
		o.RefreshInterval = 0
	}
	if o.RefreshInterval == 0 {
		o.RefreshInterval = def.RefreshInterval
	}
	return o
}

type cachedFile struct {
	meta    File
	symbols []Symbol
	modTime time.Time
	size    int64
}

type promptKey struct {
	version int64
	query   string
	tokens  int
}

// Index is a lazily built, incrementally refreshed symbol index. It is safe
// for concurrent use.
type Index struct {
	root string
	opts Options

	mu      sync.Mutex
	files   map[string]*cachedFile
	order   []string
	symbols []Symbol
	built   time.Time
	checked time.Time
	version int64
	lastErr error
	prompts map[promptKey]string
}

func New(root string, opts Options) *Index {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Index{
		root:  root,
		opts:  opts.normalized(),
		files: map[string]*cachedFile{},
	}
}

func (i *Index) Root() string { return i.root }

// Stats returns workspace-wide counts. The first call builds the index.
func (i *Index) Stats() (files, symbols int, built time.Time, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensureLocked(false); err != nil {
		return 0, 0, time.Time{}, err
	}
	return len(i.order), len(i.symbols), i.built, nil
}

// Files returns indexed files ordered by path.
func (i *Index) Files() ([]File, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensureLocked(false); err != nil {
		return nil, err
	}
	out := make([]File, 0, len(i.order))
	for _, rel := range i.order {
		out = append(out, i.files[rel].meta)
	}
	return out, nil
}

// Symbols returns every symbol ordered by file and line.
func (i *Index) Symbols() ([]Symbol, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensureLocked(false); err != nil {
		return nil, err
	}
	return append([]Symbol(nil), i.symbols...), nil
}

// SearchSymbols finds declarations by name. see searchSymbols in rank.go for
// the matching rules.
func (i *Index) SearchSymbols(query, pathPrefix, kind string, limit int) ([]Symbol, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensureLocked(false); err != nil {
		return nil, err
	}
	return searchSymbols(i.symbols, query, pathPrefix, kind, limit), nil
}

// Map renders a token-budgeted, query-ranked map of the workspace.
func (i *Index) Map(opts MapOptions) (MapResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensureLocked(false); err != nil {
		return MapResult{}, err
	}
	return renderMap(i.viewsLocked(), opts), nil
}

// Prompt returns the repository map section for the system prompt. It returns
// an empty string when maxTokens is zero or the workspace cannot be indexed.
func (i *Index) Prompt(query string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if err := i.ensureLocked(false); err != nil {
		return ""
	}
	query = strings.TrimSpace(query)
	if len(query) > 500 {
		query = query[:500]
	}
	key := promptKey{version: i.version, query: query, tokens: maxTokens}
	if cached, ok := i.prompts[key]; ok {
		return cached
	}
	section := renderPrompt(i.viewsLocked(), query, maxTokens)
	if i.prompts == nil || len(i.prompts) > 32 {
		i.prompts = map[promptKey]string{}
	}
	i.prompts[key] = section
	return section
}

func (i *Index) viewsLocked() []fileView {
	views := make([]fileView, 0, len(i.order))
	for _, rel := range i.order {
		cf := i.files[rel]
		views = append(views, fileView{meta: cf.meta, symbols: cf.symbols})
	}
	return views
}

func (i *Index) ensureLocked(force bool) error {
	if i.root == "" {
		i.lastErr = errors.New("index: workspace root is empty")
		return i.lastErr
	}
	now := time.Now()
	if !force && !i.checked.IsZero() && now.Sub(i.checked) < i.opts.RefreshInterval {
		return i.lastErr
	}
	i.checked = now

	info, err := os.Stat(i.root)
	if err != nil {
		i.lastErr = err
		return err
	}
	if !info.IsDir() {
		i.lastErr = errors.New("index: workspace root is not a directory")
		return i.lastErr
	}

	next := make(map[string]*cachedFile, len(i.files))
	order := make([]string, 0, len(i.files))
	changed := false
	walkErr := filepath.WalkDir(i.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != i.root && (util.IgnoredDir(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(i.root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !includeFile(rel) {
			return nil
		}
		if len(order) >= i.opts.MaxFiles {
			return fs.SkipAll
		}
		fi, err := d.Info()
		if err != nil || fi.Size() > i.opts.MaxFileBytes {
			return nil
		}
		if old := i.files[rel]; old != nil && old.size == fi.Size() && old.modTime.Equal(fi.ModTime()) {
			next[rel] = old
			order = append(order, rel)
			return nil
		}
		meta := File{Path: rel, Bytes: fi.Size()}
		var symbols []Symbol
		if lang := languageOf(rel); lang != "" {
			data, err := os.ReadFile(p)
			if err != nil || isBinary(data) {
				return nil
			}
			meta.Language = lang
			meta.Lines = countLines(data)
			symbols = parseSymbols(rel, lang, data)
			meta.Symbols = len(symbols)
		}
		next[rel] = &cachedFile{meta: meta, symbols: symbols, modTime: fi.ModTime(), size: fi.Size()}
		order = append(order, rel)
		changed = true
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		i.lastErr = walkErr
		return walkErr
	}
	if len(next) != len(i.files) {
		changed = true
	}

	sort.Strings(order)
	symbols := make([]Symbol, 0, len(i.symbols))
	for _, rel := range order {
		symbols = append(symbols, next[rel].symbols...)
	}
	i.files = next
	i.order = order
	i.symbols = symbols
	if changed || i.built.IsZero() {
		i.version++
		i.built = now
		i.prompts = nil
	}
	i.lastErr = nil
	return nil
}

var skipNames = map[string]bool{
	"package-lock.json": true,
	"yarn.lock":         true,
	"pnpm-lock.yaml":    true,
	"go.sum":            true,
	"Cargo.lock":        true,
	"composer.lock":     true,
	"poetry.lock":       true,
	"Gemfile.lock":      true,
	".DS_Store":         true,
}

var skipSuffixes = []string{".min.js", ".min.css", ".map"}

var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".bmp": true, ".tiff": true, ".pdf": true,
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true,
	".7z": true, ".rar": true, ".exe": true, ".dll": true, ".so": true,
	".dylib": true, ".a": true, ".o": true, ".obj": true, ".class": true,
	".jar": true, ".war": true, ".wasm": true, ".woff": true, ".woff2": true,
	".ttf": true, ".otf": true, ".eot": true, ".mp3": true, ".mp4": true,
	".wav": true, ".ogg": true, ".webm": true, ".mov": true, ".avi": true,
	".sqlite": true, ".db": true, ".pickle": true, ".pkl": true, ".npy": true,
	".npz": true, ".pt": true, ".onnx": true, ".bin": true,
}

func includeFile(rel string) bool {
	base := filepath.Base(rel)
	if skipNames[base] {
		return false
	}
	lower := strings.ToLower(base)
	ext := strings.ToLower(filepath.Ext(lower))
	if binaryExts[ext] {
		return false
	}
	for _, suffix := range skipSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return true
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := 0
	for _, b := range data {
		if b == '\n' {
			n++
		}
	}
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
