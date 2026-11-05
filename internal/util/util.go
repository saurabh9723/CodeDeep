package util

import (
	"fmt"
	"strings"
	"time"
)

// Truncate shortens s to max characters, keeping a marker.
func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max < 16 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// TruncateMiddle keeps the beginning and the end of s, which is what matters
// for compiler output and large command logs.
func TruncateMiddle(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max < 80 {
		return Truncate(s, max)
	}
	head := max * 2 / 3
	tail := max - head - 40
	if tail < 0 {
		tail = 0
	}
	return s[:head] + fmt.Sprintf("\n…[%d bytes truncated]…\n", len(s)-head-tail) + s[len(s)-tail:]
}

func HumanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func HumanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// FirstLine returns the first non-empty line of s, truncated.
func FirstLine(s string, max int) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return Truncate(line, max)
		}
	}
	return ""
}
