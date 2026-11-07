package trace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"codedeep/internal/agent"
)

// Store is a directory of trace JSON files. It is safe for concurrent saves.
type Store struct {
	Dir string
	Max int
}

func New(dir string) *Store {
	return &Store{Dir: dir, Max: DefaultMax}
}

func (s *Store) path(id string) string {
	return filepath.Join(s.Dir, id+".json")
}

// Save writes a trace, assigning an ID and created time when missing. The
// write is atomic so a viewer never reads a half-written file.
func (s *Store) Save(t Trace) (string, error) {
	if s == nil || strings.TrimSpace(s.Dir) == "" {
		return "", errors.New("trace store has no directory")
	}
	if t.ID == "" {
		t.ID = NewID(t.Kind, t.Title)
	}
	if !validID(t.ID) {
		return "", fmt.Errorf("invalid trace id %q", t.ID)
	}
	if t.Created.IsZero() {
		t.Created = time.Now().UTC()
	}
	if t.Kind == "" {
		t.Kind = KindSession
	}
	if t.Events == nil {
		t.Events = []agent.Event{}
	}
	t.EventCount = len(t.Events)
	data, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("encode trace: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.Dir, ".trace-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, s.path(t.ID)); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	s.prune()
	return t.ID, nil
}

// List returns trace headers, newest first.
func (s *Store) List() ([]Meta, error) {
	if s == nil || strings.TrimSpace(s.Dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	metas := make([]Meta, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if err != nil {
			continue
		}
		var meta Meta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].Created.Equal(metas[j].Created) {
			return metas[i].ID > metas[j].ID
		}
		return metas[i].Created.After(metas[j].Created)
	})
	return metas, nil
}

// Load reads one complete trace.
func (s *Store) Load(id string) (Trace, error) {
	if s == nil || !validID(id) {
		return Trace{}, os.ErrNotExist
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return Trace{}, err
	}
	var t Trace
	if err := json.Unmarshal(data, &t); err != nil {
		return Trace{}, fmt.Errorf("decode trace %s: %w", id, err)
	}
	if t.ID == "" {
		t.ID = id
	}
	return t, nil
}

// Delete removes one trace.
func (s *Store) Delete(id string) error {
	if s == nil || !validID(id) {
		return os.ErrNotExist
	}
	return os.Remove(s.path(id))
}

// prune keeps the newest Max traces. It is best-effort: a failed removal must
// not fail the save that triggered it.
func (s *Store) prune() {
	if s.Max <= 0 {
		return
	}
	metas, err := s.List()
	if err != nil || len(metas) <= s.Max {
		return
	}
	for _, meta := range metas[s.Max:] {
		_ = os.Remove(s.path(meta.ID))
	}
}
