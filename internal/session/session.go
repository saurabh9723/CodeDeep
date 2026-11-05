package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"codedeep/internal/llm"
	"codedeep/internal/util"
)

type Session struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Model    string        `json:"model"`
	Cwd      string        `json:"cwd"`
	Created  time.Time     `json:"created"`
	Updated  time.Time     `json:"updated"`
	Messages []llm.Message `json:"messages"`
	Usage    llm.Usage     `json:"usage"`
	Turns    int           `json:"turns"`
}

type Meta struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Model        string    `json:"model"`
	Cwd          string    `json:"cwd"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	MessageCount int       `json:"message_count"`
	TotalTokens  int       `json:"total_tokens"`
}

type Store struct {
	Dir string
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,80}$`)

func NewStore(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("session directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	return &Store{Dir: dir}, nil
}

func NewID() string {
	buf := make([]byte, 3)
	_, _ = rand.Read(buf)
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(buf)
}

func (s *Store) New(model, cwd string) *Session {
	now := time.Now().UTC()
	return &Session{
		ID:       NewID(),
		Model:    model,
		Cwd:      cwd,
		Created:  now,
		Updated:  now,
		Messages: []llm.Message{},
	}
}

func validateID(id string) error {
	if !idPattern.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

func (s *Store) path(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, id+".json"), nil
}

func (s *Store) Save(sess *Session) error {
	if sess == nil {
		return errors.New("nil session")
	}
	path, err := s.path(sess.ID)
	if err != nil {
		return err
	}
	if sess.Title == "" || (sess.Title == "New session" && hasUserMessage(sess.Messages)) {
		sess.Title = deriveTitle(sess.Messages)
	}
	sess.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) Load(id string) (*Session, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("parse session %s: %w", id, err)
	}
	sess.ID = id
	if sess.Messages == nil {
		sess.Messages = []llm.Message{}
	}
	return &sess, nil
}

func (s *Store) Delete(id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	metas := make([]Meta, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		sess, err := s.Load(id)
		if err != nil {
			continue
		}
		metas = append(metas, Meta{
			ID:           sess.ID,
			Title:        sess.Title,
			Model:        sess.Model,
			Cwd:          sess.Cwd,
			Created:      sess.Created,
			Updated:      sess.Updated,
			MessageCount: len(sess.Messages),
			TotalTokens:  sess.Usage.TotalTokens,
		})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Updated.After(metas[j].Updated) })
	return metas, nil
}

// Latest returns the most recently updated session, optionally filtered to a
// working directory.
func (s *Store) Latest(cwd string) (*Session, error) {
	metas, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		if cwd != "" && !samePath(m.Cwd, cwd) {
			continue
		}
		return s.Load(m.ID)
	}
	return nil, os.ErrNotExist
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb)) || aa == bb
}

func hasUserMessage(messages []llm.Message) bool {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if !isSummary(m.Content) {
			return true
		}
	}
	return false
}

func isSummary(content string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(content)), "[conversation summary")
}

func deriveTitle(messages []llm.Message) string {
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		text := strings.Join(strings.Fields(m.Content), " ")
		if isSummary(text) {
			continue
		}
		if text != "" {
			return util.Truncate(text, 64)
		}
	}
	return "New session"
}
