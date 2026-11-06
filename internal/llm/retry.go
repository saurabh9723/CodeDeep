package llm

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryNotice describes a failed attempt that the provider will retry. It is
// delivered as a Delta before any content so the CLI and web UI can show why
// the request is waiting.
type RetryNotice struct {
	Attempt int    `json:"attempt"`
	Max     int    `json:"max"`
	DelayMS int    `json:"delay_ms"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

const (
	retryMaxDelay = 8 * time.Second
	// maxRetryAfter caps how long a provider can ask us to wait. A longer
	// delay usually means the quota is exhausted for now.
	maxRetryAfter = 60 * time.Second
)

// retryBaseDelay is a variable so tests can shorten backoff.
var retryBaseDelay = 800 * time.Millisecond

// retryDelay returns how long to wait before the next attempt. A Retry-After
// header wins over exponential backoff; otherwise the delay doubles from
// retryBaseDelay up to retryMaxDelay with up to 10% jitter.
func retryDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			if d > maxRetryAfter {
				d = maxRetryAfter
			}
			return d
		}
	}
	d := retryBaseDelay << attempt
	if d <= 0 || d > retryMaxDelay {
		d = retryMaxDelay
	}
	jitter := time.Duration(rand.Int64N(int64(d)/5 + 1))
	d = d - d/10 + jitter
	if d > retryMaxDelay {
		d = retryMaxDelay
	}
	return d
}

// parseRetryAfter accepts both forms defined for the header: delay seconds or
// an HTTP date. A missing or malformed value reports false so callers fall
// back to exponential backoff.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil {
		if secs < 0 {
			return 0, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(value); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func retryReason(status int, err error) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate limited"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return "provider timeout"
	case status >= 500:
		return "provider unavailable"
	case status > 0:
		return fmt.Sprintf("http %d", status)
	case err != nil:
		return "connection error"
	}
	return "provider busy"
}

// notifyRetry reports a pending retry through the stream callback. The
// callback's error stops the request, matching delta handling.
func notifyRetry(onDelta func(Delta) error, attempt, max, status int, err error, delay time.Duration) error {
	if onDelta == nil {
		return nil
	}
	return onDelta(Delta{Retry: &RetryNotice{
		Attempt: attempt,
		Max:     max,
		DelayMS: int(delay / time.Millisecond),
		Status:  status,
		Message: retryReason(status, err),
	}})
}
