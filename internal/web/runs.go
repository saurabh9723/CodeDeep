package web

import (
	"context"
	"sync"
	"time"

	"codedeep/internal/agent"
)

const (
	// runBufferEvents and runBufferBytes cap the replay memory kept for one
	// active (or recently finished) run. A viewer that falls further behind
	// reconnects and resumes from the buffer or the session on disk.
	runBufferEvents = 2000
	runBufferBytes  = 4 << 20
	runSubBuffer    = 256
	// runRetention keeps a finished run around so a client that reconnects
	// just after completion can still collect the final events.
	runRetention = 2 * time.Minute
)

type queuedEvent struct {
	seq int64
	ev  agent.Event
}

// activeRun is one server-owned chat turn. It is independent of any HTTP
// connection: viewers attach and detach, while the agent keeps running.
type activeRun struct {
	id     string
	cancel context.CancelFunc

	mu       sync.Mutex
	events   []queuedEvent
	bytes    int
	seq      int64
	saved    int64
	terminal int64
	done     bool
	subs     map[int]chan queuedEvent
	nextSub  int
}

func eventSize(ev agent.Event) int {
	return len(ev.Text) + len(ev.Output) + len(ev.Diff) + len(ev.Message) + len(ev.Args) + 256
}

func (r *activeRun) emit(ev agent.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.seq++
	ev.Seq = r.seq
	q := queuedEvent{seq: r.seq, ev: ev}
	r.events = append(r.events, q)
	r.bytes += eventSize(ev)
	for len(r.events) > 1 && (len(r.events) > runBufferEvents || r.bytes > runBufferBytes) {
		r.bytes -= eventSize(r.events[0].ev)
		r.events = r.events[1:]
	}
	if ev.Type == agent.EventSaved {
		r.saved = r.seq
	}
	// Terminal events are UI signals, never persisted in the transcript, so a
	// fresh page must still receive the one for this turn.
	if ev.Depth == 0 && (ev.Type == agent.EventDone || ev.Type == agent.EventError) {
		r.terminal = r.seq
	}
	for id, ch := range r.subs {
		select {
		case ch <- q:
		default:
			// The viewer is too far behind to keep up. Drop it; it will
			// reconnect with after=<last seq it saw>.
			close(ch)
			delete(r.subs, id)
		}
	}
}

// attach registers a viewer. after < 0 resumes from the last persisted point,
// which is what a freshly loaded page needs: events before it are already part
// of the session on disk. reset reports that the buffer no longer reaches back
// to after, so the viewer should reload the session first.
func (r *activeRun) attach(after int64) (id int, replay []queuedEvent, ch chan queuedEvent, reset bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if after < 0 {
		after = r.saved
		if r.terminal > 0 && r.terminal-1 < after {
			// Complete turns persist the transcript before (or, for the
			// deferred checkpoint save, after) the terminal event. A fresh
			// page already has the transcript from disk but still needs the
			// terminal event to finalize usage and status.
			after = r.terminal - 1
		}
	}
	if len(r.events) > 0 && after+1 < r.events[0].seq {
		reset = true
	}
	for _, q := range r.events {
		if q.seq > after {
			replay = append(replay, q)
		}
	}
	id = -1
	ch = make(chan queuedEvent, runSubBuffer)
	if r.done {
		close(ch)
		return id, replay, ch, reset
	}
	id = r.nextSub
	r.nextSub++
	r.subs[id] = ch
	return id, replay, ch, reset
}

func (r *activeRun) detach(id int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.subs[id]; ok {
		close(ch)
		delete(r.subs, id)
	}
}

func (r *activeRun) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	for id, ch := range r.subs {
		close(ch)
		delete(r.subs, id)
	}
}

// finished reports whether the agent has stopped. A finished run is retained
// briefly for late viewers, but it no longer holds the session.
func (r *activeRun) finished() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

type runRegistry struct {
	mu   sync.Mutex
	runs map[string]*activeRun
}

func newRunRegistry() *runRegistry {
	return &runRegistry{runs: map[string]*activeRun{}}
}

// begin reserves the session for a new run. A finished run that is still
// retained is replaced.
func (g *runRegistry) begin(id string, cancel context.CancelFunc) (*activeRun, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.runs[id]; ok {
		existing.mu.Lock()
		done := existing.done
		existing.mu.Unlock()
		if !done {
			return nil, false
		}
		delete(g.runs, id)
	}
	r := &activeRun{id: id, cancel: cancel, subs: map[int]chan queuedEvent{}}
	g.runs[id] = r
	return r, true
}

func (g *runRegistry) get(id string) *activeRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runs[id]
}

// running reports whether the session has a live run. Retained runs that
// already finished do not hold the session.
func (g *runRegistry) running(id string) bool {
	if run := g.get(id); run != nil {
		return !run.finished()
	}
	return false
}

func (g *runRegistry) remove(r *activeRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runs[r.id] == r {
		delete(g.runs, r.id)
	}
}

func (g *runRegistry) ids() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.runs))
	for id, run := range g.runs {
		if !run.finished() {
			ids = append(ids, id)
		}
	}
	return ids
}
