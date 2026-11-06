// Package memory stores durable project notes that survive across sessions.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	maxNoteBytes = 32 << 10
	maxBodyBytes = 24 << 10
	maxNameLen   = 60
	maxDescRunes = 200
	maxResults   = 10
)

// Note is one durable memory file.
type Note struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body,omitempty"`
	Scope       string `json:"scope"`
	Path        string `json:"path,omitempty"`
}

// Store reads and writes project and global memory directories.
type Store struct {
	ProjectDir string
	GlobalDir  string
}

// New returns a store rooted at <workDir>/.codedeep/memory and
// <home>/.codedeep/memory.
func New(workDir, home string) *Store {
	s := &Store{}
	if strings.TrimSpace(workDir) != "" {
		s.ProjectDir = filepath.Join(workDir, ".codedeep", "memory")
	}
	if strings.TrimSpace(home) != "" {
		s.GlobalDir = filepath.Join(home, ".codedeep", "memory")
	}
	return s
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// NormalizeName turns a free-form name into a safe file name.
func NormalizeName(name string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '/':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > maxNameLen {
		out = strings.TrimRight(out[:maxNameLen], "-._")
	}
	if out == "" || !namePattern.MatchString(out) {
		return "", fmt.Errorf("memory name %q must contain letters or digits", name)
	}
	return out, nil
}

func (s *Store) dir(scope string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", "project":
		if s.ProjectDir == "" {
			return "", errors.New("project memory is not available")
		}
		return s.ProjectDir, nil
	case "global":
		if s.GlobalDir == "" {
			return "", errors.New("global memory is not available")
		}
		return s.GlobalDir, nil
	default:
		return "", fmt.Errorf("unknown memory scope %q (use project or global)", scope)
	}
}

func parseFile(path, scope string) (Note, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Note{}, err
	}
	if len(data) > maxNoteBytes {
		data = data[:maxNoteBytes]
	}
	note := Note{
		Name:  strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		Scope: scope,
		Path:  path,
	}
	body := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.HasPrefix(body, "---\n") {
		if end := strings.Index(body[4:], "\n---"); end >= 0 {
			front := body[4 : 4+end]
			body = strings.TrimLeft(body[4+end+4:], "\n")
			for _, line := range strings.Split(front, "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "name":
					if v, err := NormalizeName(value); err == nil {
						note.Name = v
					}
				case "description":
					note.Description = strings.TrimSpace(value)
				}
			}
		}
	}
	note.Body = strings.TrimSpace(body)
	if note.Description == "" {
		note.Description = firstLine(note.Body)
	}
	note.Description = truncateRunes(note.Description, maxDescRunes)
	return note, nil
}

func (s *Store) listDir(dir, scope string) []Note {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	notes := make([]Note, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") ||
			strings.EqualFold(entry.Name(), "MEMORY.md") {
			continue
		}
		note, err := parseFile(filepath.Join(dir, entry.Name()), scope)
		if err != nil || note.Body == "" && note.Description == "" {
			continue
		}
		notes = append(notes, note)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Name < notes[j].Name })
	return notes
}

// List returns project notes followed by global notes. A project note hides a
// global note with the same name.
func (s *Store) List() []Note {
	seen := map[string]bool{}
	var out []Note
	for _, n := range s.listDir(s.ProjectDir, "project") {
		if seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		out = append(out, n)
	}
	for _, n := range s.listDir(s.GlobalDir, "global") {
		if seen[n.Name] {
			continue
		}
		seen[n.Name] = true
		out = append(out, n)
	}
	return out
}

// Get returns a note from one scope.
func (s *Store) Get(scope, name string) (Note, bool) {
	dir, err := s.dir(scope)
	if err != nil {
		return Note{}, false
	}
	normalized, err := NormalizeName(name)
	if err != nil {
		return Note{}, false
	}
	note, err := parseFile(filepath.Join(dir, normalized+".md"), normalizeScope(scope))
	if err != nil {
		return Note{}, false
	}
	return note, true
}

// Find looks for a note in the project scope first, then global.
func (s *Store) Find(name string) (Note, bool) {
	if note, ok := s.Get("project", name); ok {
		return note, true
	}
	return s.Get("global", name)
}

// Save writes a note and returns the stored form.
func (s *Store) Save(scope, name, description, body string) (Note, error) {
	dir, err := s.dir(scope)
	if err != nil {
		return Note{}, err
	}
	normalized, err := NormalizeName(name)
	if err != nil {
		return Note{}, err
	}
	body = strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	if body == "" {
		return Note{}, errors.New("memory body is empty")
	}
	if len(body) > maxBodyBytes {
		return Note{}, fmt.Errorf("memory body is %d bytes; the limit is %d", len(body), maxBodyBytes)
	}
	desc := strings.Join(strings.Fields(description), " ")
	if desc == "" {
		desc = firstLine(body)
	}
	desc = truncateRunes(desc, maxDescRunes)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Note{}, fmt.Errorf("create memory directory: %w", err)
	}
	path := filepath.Join(dir, normalized+".md")
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", normalized, desc, body)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return Note{}, fmt.Errorf("write memory: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return Note{}, fmt.Errorf("write memory: %w", err)
	}
	note, err := parseFile(path, normalizeScope(scope))
	if err != nil {
		return Note{}, err
	}
	return note, nil
}

// Delete removes a note from one scope.
func (s *Store) Delete(scope, name string) (Note, error) {
	dir, err := s.dir(scope)
	if err != nil {
		return Note{}, err
	}
	normalized, err := NormalizeName(name)
	if err != nil {
		return Note{}, err
	}
	path := filepath.Join(dir, normalized+".md")
	note, err := parseFile(path, normalizeScope(scope))
	if err != nil {
		return Note{}, fmt.Errorf("memory %q not found in %s scope", normalized, normalizeScope(scope))
	}
	if err := os.Remove(path); err != nil {
		return Note{}, fmt.Errorf("delete memory: %w", err)
	}
	return note, nil
}

func normalizeScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "global") {
		return "global"
	}
	return "project"
}

// Search returns notes matching query. An empty query lists every note.
func (s *Store) Search(query string) []Note {
	notes := s.List()
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return notes
	}
	words := strings.Fields(q)
	type hit struct {
		note  Note
		score int
	}
	var hits []hit
	for _, note := range notes {
		haystack := strings.ToLower(note.Name + "\n" + note.Description + "\n" + note.Body + "\n" + note.Scope)
		score := 0
		if strings.Contains(haystack, q) {
			score += 10
		}
		for _, w := range words {
			if len(w) >= 3 && strings.Contains(haystack, w) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{note: note, score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].note.Name < hits[j].note.Name
	})
	if len(hits) > maxResults {
		hits = hits[:maxResults]
	}
	out := make([]Note, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.note)
	}
	return out
}

// PromptSection renders a bounded index for the system prompt.
func (s *Store) PromptSection(maxChars int) string {
	notes := s.List()
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Project memory\n")
	b.WriteString("Durable notes saved by earlier sessions in this workspace and globally. " +
		"Use recall to read a note in full; save new durable facts with remember.\n")
	for _, note := range notes {
		line := fmt.Sprintf("- %s [%s] — %s\n", note.Name, note.Scope, note.Description)
		if b.Len()+len(line) > maxChars {
			b.WriteString("- [more memory notes omitted]\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		if line != "" {
			return truncateRunes(line, maxDescRunes)
		}
	}
	return ""
}

func truncateRunes(text string, max int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= max {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:max-1])) + "…"
}
