package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codedeep/internal/agent"
	"codedeep/internal/app"
	"codedeep/internal/config"
	"codedeep/internal/llm"
)

func TestRunAttachResumesFromSavedPoint(t *testing.T) {
	run := &activeRun{id: "s1", subs: map[int]chan queuedEvent{}}
	run.emit(agent.Event{Type: "session"})
	run.emit(agent.Event{Type: "user", Text: "hi"})
	run.emit(agent.Event{Type: agent.EventSaved})
	run.emit(agent.Event{Type: "text", Text: "hello"})

	cases := []struct {
		name  string
		after int64
		want  int64
	}{
		{"fresh page resumes after saved", -1, 4},
		{"reconnect resumes after last seen", 2, 3},
		{"new viewer replays everything", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, replay, _, reset := run.attach(tc.after)
			defer run.detach(id)
			if reset {
				t.Fatal("unexpected reset")
			}
			if len(replay) == 0 || replay[0].seq != tc.want {
				t.Fatalf("replay = %+v, want first seq %d", replay, tc.want)
			}
		})
	}
}

func TestRunAttachResetsWhenBufferOverflows(t *testing.T) {
	run := &activeRun{id: "s1", subs: map[int]chan queuedEvent{}}
	for i := 0; i < runBufferEvents+1; i++ {
		run.emit(agent.Event{Type: "text"})
	}
	_, _, _, reset := run.attach(0)
	if !reset {
		t.Fatal("want reset when the buffer no longer reaches the requested seq")
	}
}

func TestRunDropsSlowSubscriber(t *testing.T) {
	run := &activeRun{id: "s1", subs: map[int]chan queuedEvent{}}
	_, _, ch, _ := run.attach(0)
	for i := 0; i < runSubBuffer+50; i++ {
		run.emit(agent.Event{Type: "text"})
	}
	got := 0
	for range ch {
		got++
	}
	if got != runSubBuffer {
		t.Fatalf("buffered %d events before dropping the viewer, want %d", got, runSubBuffer)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.subs) != 0 {
		t.Fatalf("slow viewer was not detached: %d subscribers", len(run.subs))
	}
}

func TestRunFinishedReplaysAndCloses(t *testing.T) {
	run := &activeRun{id: "s1", subs: map[int]chan queuedEvent{}}
	run.emit(agent.Event{Type: "session"})
	run.emit(agent.Event{Type: "done"})
	run.finish()

	id, replay, ch, reset := run.attach(0)
	if id != -1 {
		t.Fatalf("finished run registered a subscriber: id %d", id)
	}
	if reset || len(replay) != 2 {
		t.Fatalf("replay = %+v, reset = %v", replay, reset)
	}
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel should be closed for a finished run")
	}
}

func TestRunRegistryReplacesFinishedRun(t *testing.T) {
	reg := newRunRegistry()
	r1, ok := reg.begin("s1", func() {})
	if !ok {
		t.Fatal("first run was rejected")
	}
	if _, ok := reg.begin("s1", func() {}); ok {
		t.Fatal("a second run was allowed while the first is active")
	}
	if ids := reg.ids(); len(ids) != 1 || ids[0] != "s1" {
		t.Fatalf("ids = %v", ids)
	}
	r1.finish()
	r2, ok := reg.begin("s1", func() {})
	if !ok || r2 == r1 {
		t.Fatalf("finished run was not replaced: ok=%v same=%v", ok, r2 == r1)
	}
	reg.remove(r1)
	if reg.get("s1") != r2 {
		t.Fatal("stale remove deleted the active run")
	}
	reg.remove(r2)
	if reg.get("s1") != nil {
		t.Fatal("run was not removed")
	}
}

// scriptedProvider streams fixed chunks. When started is set it blocks before
// chunk gateAt until release is closed or the context is canceled, which lets
// tests observe a run that is still in flight.
type scriptedProvider struct {
	chunks  []string
	gateAt  int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *scriptedProvider) gate(ctx context.Context) error {
	if p.started == nil {
		return nil
	}
	p.once.Do(func() { close(p.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		return nil
	}
}

func (p *scriptedProvider) Stream(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Delta) error) (*llm.Response, error) {
	var text strings.Builder
	for i, chunk := range p.chunks {
		if i == p.gateAt {
			if err := p.gate(ctx); err != nil {
				return nil, err
			}
		}
		text.WriteString(chunk)
		if err := onDelta(llm.Delta{Content: chunk}); err != nil {
			return nil, err
		}
	}
	return &llm.Response{
		Message:      llm.Message{Role: "assistant", Content: text.String()},
		FinishReason: "stop",
	}, nil
}

func (p *scriptedProvider) Complete(ctx context.Context, req llm.ChatRequest) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: "summary"}}, nil
}

func (p *scriptedProvider) ListModels(ctx context.Context) ([]string, error) {
	return []string{"test-model"}, nil
}

func newRunTestServer(t *testing.T, provider llm.Provider) *Server {
	t.Helper()
	cfg := &config.Config{
		APIKey:      "test-key",
		Model:       "test-model",
		WorkDir:     t.TempDir(),
		SessionsDir: t.TempDir(),
		TracesDir:   t.TempDir(),
		Home:        t.TempDir(),
		MaxSteps:    3,
		Approval:    "ask",
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(a.Close)
	a.Client = provider
	return New(a)
}

func scanEvents(r io.Reader, until string) ([]agent.Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var out []agent.Event
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev agent.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return out, fmt.Errorf("decode event %q: %w", line, err)
		}
		out = append(out, ev)
		if ev.Type == until {
			return out, nil
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	types := make([]string, len(out))
	for i, ev := range out {
		types[i] = ev.Type
	}
	return out, fmt.Errorf("stream ended before %q; got %v", until, types)
}

func mustScanEvents(t *testing.T, r io.Reader, until string) []agent.Event {
	t.Helper()
	events, err := scanEvents(r, until)
	if err != nil {
		t.Fatalf("scan events: %v", err)
	}
	return events
}

func eventTexts(events []agent.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == agent.EventText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func TestRunSurvivesDisconnectAndReattaches(t *testing.T) {
	provider := &scriptedProvider{
		chunks:  []string{"hello ", "world", "!"},
		gateAt:  1,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	s := newRunTestServer(t, provider)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/api/chat", strings.NewReader(`{"prompt":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	sessionID := resp.Header.Get("X-Session-Id")
	if sessionID == "" {
		t.Fatal("missing X-Session-Id header")
	}

	// Read the first text delta, then drop the connection like a lost tab.
	var lastSeq int64
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev agent.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		lastSeq = ev.Seq
		if ev.Type == agent.EventText {
			break
		}
	}
	cancel()
	resp.Body.Close()

	// The run is still owned by the server and still in flight.
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never reached the gate")
	}
	getResp, err := http.Get(ts.URL + "/api/sessions/" + sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	var sess struct {
		ID      string `json:"id"`
		Running bool   `json:"running"`
	}
	if err := json.NewDecoder(getResp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	getResp.Body.Close()
	if !sess.Running {
		t.Fatal("session is not reported as running after the viewer disconnected")
	}

	bootResp, err := http.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var boot struct {
		Running []string `json:"running_sessions"`
	}
	if err := json.NewDecoder(bootResp.Body).Decode(&boot); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	bootResp.Body.Close()
	found := false
	for _, id := range boot.Running {
		if id == sessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("bootstrap running_sessions = %v, want %s", boot.Running, sessionID)
	}

	close(provider.release)

	streamResp, err := http.Get(fmt.Sprintf("%s/api/sessions/%s/stream?after=%d", ts.URL, sessionID, lastSeq))
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	defer streamResp.Body.Close()
	events := mustScanEvents(t, streamResp.Body, agent.EventDone)
	_, _ = io.Copy(io.Discard, streamResp.Body) // wait for the server to finish the run
	text := eventTexts(events)
	if !strings.Contains("hello world!", text) {
		t.Fatalf("replayed text = %q, want the rest of the answer", text)
	}
	if strings.Contains(text, "hello hello") {
		t.Fatalf("replay duplicated already seen content: %q", text)
	}

	// A freshly loaded page resumes after the persisted point, so it does not
	// render the answer twice on top of history loaded from disk, but it still
	// receives the terminal event for the turn.
	freshResp, err := http.Get(ts.URL + "/api/sessions/" + sessionID + "/stream")
	if err != nil {
		t.Fatalf("fresh stream: %v", err)
	}
	fresh := mustScanEvents(t, freshResp.Body, agent.EventDone)
	freshResp.Body.Close()
	if text := eventTexts(fresh); text != "" {
		t.Fatalf("fresh page replayed text that is already on disk: %q", text)
	}

	saved, err := s.App.Sessions.Load(sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	var assistantText string
	for _, m := range saved.Messages {
		if m.Role == "assistant" {
			assistantText = m.Content
		}
	}
	if assistantText != "hello world!" {
		t.Fatalf("persisted assistant text = %q", assistantText)
	}

	getResp, err = http.Get(ts.URL + "/api/sessions/" + sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	sess.Running = true
	if err := json.NewDecoder(getResp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	getResp.Body.Close()
	if sess.Running {
		t.Fatal("session is still reported as running after the turn finished")
	}
}

func TestChatRejectsConcurrentRunAndStopStops(t *testing.T) {
	provider := &scriptedProvider{
		chunks:  []string{"never sent"},
		gateAt:  0,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	s := newRunTestServer(t, provider)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	type chatResult struct {
		sessionID string
		events    []agent.Event
		err       error
	}
	results := make(chan chatResult, 1)
	ids := make(chan string, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/api/chat", "application/json", strings.NewReader(`{"prompt":"hi"}`))
		if err != nil {
			results <- chatResult{err: err}
			return
		}
		defer resp.Body.Close()
		id := resp.Header.Get("X-Session-Id")
		ids <- id
		events, err := scanEvents(resp.Body, agent.EventDone)
		_, _ = io.Copy(io.Discard, resp.Body) // wait for the run to be released
		results <- chatResult{sessionID: id, events: events, err: err}
	}()

	var sessionID string
	select {
	case sessionID = <-ids:
	case <-time.After(5 * time.Second):
		t.Fatal("chat never started")
	}
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started")
	}
	if sessionID == "" {
		t.Fatal("missing X-Session-Id header")
	}

	resp, err := http.Post(ts.URL+"/api/chat", "application/json",
		strings.NewReader(fmt.Sprintf(`{"session_id":%q,"prompt":"again"}`, sessionID)))
	if err != nil {
		t.Fatalf("second chat: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent chat status = %d, want 409", resp.StatusCode)
	}

	stopResp, err := http.Post(ts.URL+"/api/sessions/"+sessionID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	var stopBody map[string]any
	if err := json.NewDecoder(stopResp.Body).Decode(&stopBody); err != nil {
		t.Fatalf("decode stop: %v", err)
	}
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK || stopBody["stopped"] != true {
		t.Fatalf("stop = %d %v", stopResp.StatusCode, stopBody)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("stream after stop: %v", res.err)
		}
		var done *agent.Event
		for i := range res.events {
			if res.events[i].Type == agent.EventDone {
				done = &res.events[i]
			}
		}
		if done == nil || done.Message != "stopped" {
			t.Fatalf("done event = %+v, want stopped", done)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chat did not finish after stop")
	}

	// The finished runner is retained briefly, but a new turn is allowed.
	provider2 := &scriptedProvider{chunks: []string{"second"}}
	s.App.Client = provider2
	resp, err = http.Post(ts.URL+"/api/chat", "application/json",
		strings.NewReader(fmt.Sprintf(`{"session_id":%q,"prompt":"again"}`, sessionID)))
	if err != nil {
		t.Fatalf("follow-up chat: %v", err)
	}
	defer resp.Body.Close()
	events := mustScanEvents(t, resp.Body, agent.EventDone)
	if text := eventTexts(events); text != "second" {
		t.Fatalf("follow-up text = %q", text)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	stopResp, err = http.Post(ts.URL+"/api/sessions/"+sessionID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop idle session: %v", err)
	}
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusConflict {
		t.Fatalf("stopping a finished run = %d, want 409", stopResp.StatusCode)
	}
}

func TestStreamAndStopUnknownRun(t *testing.T) {
	s := newRunTestServer(t, &scriptedProvider{chunks: []string{"x"}})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/sessions", "application/json", nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	resp.Body.Close()

	streamResp, err := http.Get(ts.URL + "/api/sessions/" + sess.ID + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusNotFound {
		t.Fatalf("stream status = %d, want 404", streamResp.StatusCode)
	}

	stopResp, err := http.Post(ts.URL+"/api/sessions/"+sess.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusConflict {
		t.Fatalf("stop status = %d, want 409", stopResp.StatusCode)
	}
}

func TestRunFreshAttachStillReplaysTerminalAfterSave(t *testing.T) {
	run := &activeRun{id: "s1", subs: map[int]chan queuedEvent{}}
	run.emit(agent.Event{Type: "session"})
	run.emit(agent.Event{Type: "user"})
	run.emit(agent.Event{Type: agent.EventSaved})
	run.emit(agent.Event{Type: agent.EventText, Text: "answer"})
	run.emit(agent.Event{Type: agent.EventDone})
	run.emit(agent.Event{Type: agent.EventSaved}) // checkpoint save after done

	_, replay, _, _ := run.attach(-1)
	if len(replay) != 2 || replay[0].ev.Type != agent.EventDone {
		t.Fatalf("replay = %+v, want the terminal event first", replay)
	}
	for _, q := range replay {
		if q.ev.Type == agent.EventText {
			t.Fatal("fresh attach replayed text that is already persisted")
		}
	}
}
