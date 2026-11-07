package trace

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"codedeep/internal/agent"
)

func sampleTrace(id, kind, title string, created time.Time) Trace {
	passed := kind == KindEval
	return Trace{
		Meta: Meta{
			ID:      id,
			Kind:    kind,
			Title:   title,
			Created: created,
			Passed:  &passed,
		},
		Prompt: "do the thing",
		Events: []agent.Event{
			{Type: agent.EventUser, Time: created.Format(time.RFC3339Nano)},
			{Type: agent.EventText, Text: "done"},
		},
		Verifier: &Verifier{Command: "go test ./...", ExitCode: 0},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	store := New(t.TempDir())
	created := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	id, err := store.Save(sampleTrace("", KindEval, "go-bug-fix", created))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id == "" || strings.ContainsAny(id, `/\`) {
		t.Fatalf("unsafe generated id %q", id)
	}
	got, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Kind != KindEval || got.Title != "go-bug-fix" || len(got.Events) != 2 {
		t.Fatalf("unexpected trace: %+v", got.Meta)
	}
	if got.EventCount != 2 {
		t.Fatalf("event_count = %d, want 2", got.EventCount)
	}
	if got.Verifier == nil || got.Verifier.Command != "go test ./..." {
		t.Fatalf("verifier not preserved: %+v", got.Verifier)
	}
	if got.Passed == nil || !*got.Passed {
		t.Fatalf("passed flag not preserved")
	}
	if !got.Created.Equal(created) {
		t.Fatalf("created = %v, want %v", got.Created, created)
	}
}

func TestStoreListOrderAndFiltering(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	base := time.Now().UTC().Truncate(time.Second)
	older := sampleTrace("older", KindSession, "older run", base.Add(-time.Hour))
	newer := sampleTrace("newer", KindEval, "newer run", base)
	for _, tr := range []Trace{older, newer} {
		if _, err := store.Save(tr); err != nil {
			t.Fatalf("save %s: %v", tr.ID, err)
		}
	}
	if err := os.WriteFile(dir+"/notes.txt", []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d traces, want 2", len(metas))
	}
	if metas[0].ID != "newer" || metas[1].ID != "older" {
		t.Fatalf("wrong order: %s, %s", metas[0].ID, metas[1].ID)
	}
	if metas[0].EventCount != 2 || metas[0].Title != "newer run" {
		t.Fatalf("header not decoded: %+v", metas[0])
	}
}

func TestStoreDelete(t *testing.T) {
	store := New(t.TempDir())
	id, err := store.Save(sampleTrace("", KindSession, "delete me", time.Time{}))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Load(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load after delete: %v", err)
	}
	if err := store.Delete(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestStoreRejectsUnsafeIDs(t *testing.T) {
	store := New(t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", `..\escape`, "a/b"} {
		if _, err := store.Load(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("load(%q) err = %v, want not-exist", id, err)
		}
		if err := store.Delete(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("delete(%q) err = %v, want not-exist", id, err)
		}
	}
	if _, err := store.Save(Trace{Meta: Meta{ID: "../escape", Kind: KindSession}, Events: []agent.Event{}}); err == nil {
		t.Fatal("save accepted an unsafe id")
	}
}

func TestStorePruneKeepsNewest(t *testing.T) {
	store := New(t.TempDir())
	store.Max = 3
	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 6; i++ {
		tr := sampleTrace(NewID("session", "prune"), KindSession, "prune", base.Add(time.Duration(i)*time.Minute))
		tr.ID = "trace-" + string(rune('a'+i))
		if _, err := store.Save(tr); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	metas, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("kept %d traces, want 3", len(metas))
	}
	if metas[0].ID != "trace-f" || metas[2].ID != "trace-d" {
		t.Fatalf("wrong survivors: %+v", metas)
	}
}

func TestNewIDIsSortableAndSafe(t *testing.T) {
	id := NewID(KindEval, "Multi File Edit / run 2")
	parts := strings.Split(id, "-")
	if len(parts) < 5 {
		t.Fatalf("unexpected id shape %q", id)
	}
	if !validID(id) {
		t.Fatalf("generated id is not valid: %q", id)
	}
	if other := NewID(KindEval, "Multi File Edit / run 2"); other == id {
		t.Fatal("ids are not unique")
	}
}
