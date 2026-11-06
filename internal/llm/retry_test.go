package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		value  string
		want   time.Duration
		wantOK bool
	}{
		{name: "seconds", value: "2", want: 2 * time.Second, wantOK: true},
		{name: "zero", value: "0", want: 0, wantOK: true},
		{name: "negative", value: "-4", want: 0, wantOK: true},
		{name: "http date", value: now.Add(3 * time.Second).Format(http.TimeFormat), want: 3 * time.Second, wantOK: true},
		{name: "past date", value: now.Add(-time.Minute).Format(http.TimeFormat), want: 0, wantOK: true},
		{name: "empty", value: "", wantOK: false},
		{name: "garbage", value: "soon", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value, now)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, %v; want %v, %v", tc.value, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func retryResponse(header string) *http.Response {
	h := http.Header{}
	if header != "" {
		h.Set("Retry-After", header)
	}
	return &http.Response{Header: h}
}

func TestRetryDelayHonorsRetryAfter(t *testing.T) {
	if got := retryDelay(0, retryResponse("5")); got != 5*time.Second {
		t.Fatalf("delay = %v, want 5s", got)
	}
	if got := retryDelay(0, retryResponse("1000")); got != maxRetryAfter {
		t.Fatalf("long delay = %v, want cap %v", got, maxRetryAfter)
	}
}

func TestRetryDelayBacksOffWithJitter(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = 100 * time.Millisecond
	defer func() { retryBaseDelay = old }()

	for attempt, base := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond} {
		got := retryDelay(attempt, nil)
		low, high := base-base/10, base+base/10
		if got < low || got > high {
			t.Errorf("attempt %d: delay = %v, want %v..%v", attempt, got, low, high)
		}
	}
	if got := retryDelay(20, nil); got < retryMaxDelay-retryMaxDelay/10 || got > retryMaxDelay {
		t.Errorf("capped delay = %v, want up to %v", got, retryMaxDelay)
	}
}

func TestStreamRetriesRateLimited(t *testing.T) {
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = old }()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test")
	var notices []*RetryNotice
	var text strings.Builder
	resp, err := client.Stream(context.Background(), ChatRequest{
		Model:    "mock",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(d Delta) error {
		if d.Retry != nil {
			notices = append(notices, d.Retry)
		}
		text.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if len(notices) != 1 {
		t.Fatalf("retry notices = %d, want 1", len(notices))
	}
	n := notices[0]
	if n.Attempt != 1 || n.Max != 4 || n.Status != http.StatusTooManyRequests || n.Message != "rate limited" {
		t.Fatalf("notice = %+v", n)
	}
	if n.DelayMS != 0 {
		t.Fatalf("delay = %dms, want 0ms from Retry-After", n.DelayMS)
	}
	if text.String() != "ok" {
		t.Fatalf("content = %q, want %q", text.String(), "ok")
	}
	if resp.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestStreamDoesNotRetryClientErrors(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "nope")
	_, err := client.Stream(context.Background(), ChatRequest{Model: "mock"}, nil)
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err = %v, want API error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestStreamRetryNoticeStopsOnCallbackError(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test")
	stop := fmt.Errorf("stop now")
	_, err := client.Stream(context.Background(), ChatRequest{Model: "mock"}, func(d Delta) error {
		if d.Retry != nil {
			return stop
		}
		return nil
	})
	if err != stop {
		t.Fatalf("err = %v, want callback error", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
