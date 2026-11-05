package session

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"codedeep/internal/llm"
)

func seedSessions(t *testing.T, st *Store) (*Session, *Session) {
	t.Helper()
	first := st.New("deepseek-chat", t.TempDir())
	first.Title = "Parser bug"
	first.Messages = []llm.Message{
		{Role: "user", Content: "The parser crashes on empty input"},
		{Role: "assistant", Content: "I will inspect the tokenizer."},
	}
	if err := st.Save(first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second := st.New("deepseek-chat", t.TempDir())
	second.Title = "Docs cleanup"
	second.Messages = []llm.Message{
		{Role: "user", Content: "Rewrite the README introduction"},
	}
	if err := st.Save(second); err != nil {
		t.Fatal(err)
	}
	return first, second
}

func TestSearchMatchesTitleAndBody(t *testing.T) {
	st := newTestStore(t)
	first, second := seedSessions(t, st)

	metas, err := st.Search("parser")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != first.ID {
		t.Fatalf("Search(parser) = %+v", metas)
	}
	if metas[0].Snippet == "" || !strings.Contains(strings.ToLower(metas[0].Snippet), "parser") {
		t.Errorf("expected a snippet containing the match, got %q", metas[0].Snippet)
	}

	metas, err = st.Search("tokenizer")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != first.ID {
		t.Fatalf("Search(tokenizer) = %+v", metas)
	}

	metas, err = st.Search("readme")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].ID != second.ID {
		t.Fatalf("Search(readme) = %+v", metas)
	}

	metas, err = st.Search("parser introduction")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("AND search should require every term, got %+v", metas)
	}

	if _, err := st.Search("no-such-term"); err != nil {
		t.Fatal(err)
	}
}

func TestSearchRanksTitleMatchesFirst(t *testing.T) {
	st := newTestStore(t)
	body := st.New("m", t.TempDir())
	body.Title = "Investigate crash"
	body.Messages = []llm.Message{{Role: "user", Content: "the tokenizer breaks"}}
	if err := st.Save(body); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	title := st.New("m", t.TempDir())
	title.Title = "Tokenizer rewrite"
	title.Messages = []llm.Message{{Role: "user", Content: "unrelated"}}
	if err := st.Save(title); err != nil {
		t.Fatal(err)
	}

	metas, err := st.Search("tokenizer")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("Search returned %d sessions", len(metas))
	}
	if metas[0].ID != title.ID {
		t.Errorf("title match should rank first, got %s (%s)", metas[0].Title, metas[0].ID)
	}
}

func TestSearchEmptyQueryListsAll(t *testing.T) {
	st := newTestStore(t)
	seedSessions(t, st)
	metas, err := st.Search("  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("empty query returned %d sessions", len(metas))
	}
}

func TestResolveByTitlePrefixAndAmbiguity(t *testing.T) {
	st := newTestStore(t)
	first, second := seedSessions(t, st)

	got, err := st.Resolve("Parser bug")
	if err != nil || got.ID != first.ID {
		t.Fatalf("Resolve exact title = %v, %v", got, err)
	}
	got, err = st.Resolve("parser BUG")
	if err != nil || got.ID != first.ID {
		t.Fatalf("Resolve should be case-insensitive: %v, %v", got, err)
	}
	got, err = st.Resolve("Docs")
	if err != nil || got.ID != second.ID {
		t.Fatalf("Resolve title substring = %v, %v", got, err)
	}
	got, err = st.Resolve(first.ID)
	if err != nil || got.ID != first.ID {
		t.Fatalf("Resolve exact id = %v, %v", got, err)
	}
	prefixLen := 6
	for prefixLen < len(first.ID) && strings.HasPrefix(second.ID, first.ID[:prefixLen]) {
		prefixLen++
	}
	got, err = st.Resolve(first.ID[:prefixLen])
	if err != nil || got.ID != first.ID {
		t.Fatalf("Resolve id prefix = %v, %v", got, err)
	}

	dupe := st.New("m", t.TempDir())
	dupe.Title = "Docs cleanup"
	if err := st.Save(dupe); err != nil {
		t.Fatal(err)
	}
	_, err = st.Resolve("Docs cleanup")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("Resolve ambiguous title = %v", err)
	}
	if _, err := st.Resolve("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Resolve missing = %v, want ErrNotExist", err)
	}
	if _, err := st.Resolve(" "); err == nil {
		t.Fatal("Resolve should reject an empty reference")
	}
}

func TestExportMarkdownAndJSON(t *testing.T) {
	st := newTestStore(t)
	sess := st.New("deepseek-chat", t.TempDir())
	sess.Title = "Parser bug"
	sess.Messages = []llm.Message{
		{Role: "user", Content: "Fix it"},
		{Role: "assistant", Content: "Done", ToolCalls: []llm.ToolCall{{
			ID: "c1", Type: "function",
			Function: llm.FunctionCall{Name: "read", Arguments: `{"path":"a.go"}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "package a"},
	}
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}

	data, ctype, name, err := Export(loaded, "md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# Parser bug", "## User", "Fix it", "## Assistant", "**Tool call:** `read`", `"path": "a.go"`, "package a"} {
		if !strings.Contains(text, want) {
			t.Errorf("markdown export missing %q", want)
		}
	}
	if !strings.HasPrefix(ctype, "text/markdown") {
		t.Errorf("content type = %q", ctype)
	}
	if !strings.HasSuffix(name, ".md") || !strings.Contains(name, "parser-bug") {
		t.Errorf("filename = %q", name)
	}

	data, ctype, name, err = Export(loaded, "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ctype, "application/json") || !strings.HasSuffix(name, ".json") {
		t.Errorf("json export = %q %q", ctype, name)
	}
	var round Session
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("json export does not round-trip: %v", err)
	}
	if round.ID != sess.ID || len(round.Messages) != 3 {
		t.Errorf("round-trip = %+v", round)
	}

	if _, _, _, err := Export(loaded, "pdf"); err == nil {
		t.Error("unsupported format should fail")
	}
}
