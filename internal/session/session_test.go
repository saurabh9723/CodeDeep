package session

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"codedeep/internal/llm"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func TestSaveLoadRoundTrip(t *testing.T) {
	st := newTestStore(t)
	sess := st.New("deepseek-chat", `C:\work`)
	sess.Messages = []llm.Message{
		{Role: "user", Content: "Fix the parser bug"},
		{Role: "assistant", Content: "Looking at it."},
	}
	sess.Usage = llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	sess.Turns = 2

	if err := st.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if sess.Title != "Fix the parser bug" {
		t.Errorf("title = %q", sess.Title)
	}

	got, err := st.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != sess.ID || got.Model != sess.Model || got.Cwd != sess.Cwd {
		t.Errorf("metadata mismatch: %+v", got)
	}
	if len(got.Messages) != 2 || got.Messages[0].Content != "Fix the parser bug" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if got.Usage.TotalTokens != 15 || got.Turns != 2 {
		t.Errorf("usage/turns = %+v / %d", got.Usage, got.Turns)
	}
	if got.Messages == nil {
		t.Error("messages should never be nil after Load")
	}
}

func TestSaveDerivesTitleOnce(t *testing.T) {
	st := newTestStore(t)
	sess := st.New("m", t.TempDir())
	sess.Messages = []llm.Message{{Role: "user", Content: "  Fix\n  the   bug  "}}
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	if sess.Title != "Fix the bug" {
		t.Errorf("title = %q, want collapsed whitespace", sess.Title)
	}

	sess.Title = "Custom title"
	sess.Messages = append(sess.Messages, llm.Message{Role: "user", Content: "another request"})
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	if sess.Title != "Custom title" {
		t.Errorf("explicit title was overwritten: %q", sess.Title)
	}
}

func TestTitleIgnoresCompactionSummary(t *testing.T) {
	st := newTestStore(t)
	sess := st.New("m", t.TempDir())
	sess.Messages = []llm.Message{{
		Role:    "user",
		Content: "[Conversation summary — earlier turns were compacted]\n\nstuff",
	}}
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	if sess.Title != "New session" {
		t.Errorf("summary-only session title = %q, want %q", sess.Title, "New session")
	}

	sess.Messages = append(sess.Messages, llm.Message{Role: "user", Content: "Real request"})
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	if sess.Title != "Real request" {
		t.Errorf("title = %q, want %q", sess.Title, "Real request")
	}
}

func TestTitleTruncatesLongPrompt(t *testing.T) {
	st := newTestStore(t)
	sess := st.New("m", t.TempDir())
	sess.Messages = []llm.Message{{Role: "user", Content: strings.Repeat("a", 200)}}
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(sess.Title) != 64 || !strings.HasSuffix(sess.Title, "…") {
		t.Errorf("truncated title = %q (runes %d)", sess.Title, utf8.RuneCountInString(sess.Title))
	}
}

func TestInvalidIDs(t *testing.T) {
	st := newTestStore(t)
	for _, id := range []string{"", "../evil", "a/b", "..", "bad id", "x..y"} {
		sess := st.New("m", t.TempDir())
		sess.ID = id
		if err := st.Save(sess); err == nil {
			t.Errorf("Save(%q) should fail", id)
		}
		if _, err := st.Load(id); err == nil {
			t.Errorf("Load(%q) should fail", id)
		}
		if err := st.Delete(id); err == nil {
			t.Errorf("Delete(%q) should fail", id)
		}
	}
}

func TestListSortsByUpdatedAndLatestFiltersCwd(t *testing.T) {
	st := newTestStore(t)
	dirA := t.TempDir()
	dirB := t.TempDir()

	first := st.New("m", dirA)
	first.Messages = []llm.Message{{Role: "user", Content: "alpha"}}
	if err := st.Save(first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second := st.New("m", dirB)
	second.Messages = []llm.Message{{Role: "user", Content: "beta"}}
	if err := st.Save(second); err != nil {
		t.Fatal(err)
	}

	metas, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("List returned %d sessions", len(metas))
	}
	if metas[0].ID != second.ID {
		t.Errorf("List not sorted newest-first: %s before %s", metas[0].ID, metas[1].ID)
	}
	if metas[0].MessageCount != 1 || metas[0].Title != "beta" {
		t.Errorf("meta = %+v", metas[0])
	}

	latestA, err := st.Latest(dirA)
	if err != nil {
		t.Fatalf("Latest(dirA): %v", err)
	}
	if latestA.ID != first.ID {
		t.Errorf("Latest(dirA) = %s, want %s", latestA.ID, first.ID)
	}
	latestAny, err := st.Latest("")
	if err != nil {
		t.Fatalf("Latest(\"\"): %v", err)
	}
	if latestAny.ID != second.ID {
		t.Errorf("Latest(\"\") = %s, want %s", latestAny.ID, second.ID)
	}
	if _, err := st.Latest(t.TempDir()); !os.IsNotExist(err) {
		t.Errorf("Latest(unknown cwd) = %v, want ErrNotExist", err)
	}
}

func TestDeleteRemovesSession(t *testing.T) {
	st := newTestStore(t)
	sess := st.New("m", t.TempDir())
	if err := st.Save(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Load(sess.ID); err == nil {
		t.Error("Load should fail after Delete")
	}
}

func TestNewIDIsValidAndUnique(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b {
		t.Fatal("NewID returned duplicate ids")
	}
	if err := validateID(a); err != nil {
		t.Errorf("NewID returned invalid id %q: %v", a, err)
	}
}

func TestNewStoreEmptyDir(t *testing.T) {
	if _, err := NewStore("  "); err == nil {
		t.Fatal("NewStore should reject an empty directory")
	}
}
