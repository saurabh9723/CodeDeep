package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/util"
)

const summarizerPrompt = `You summarize conversations between a user and a coding agent so the agent can continue working with limited context.
Preserve, in order of importance:
- the user's goals, constraints, and corrections
- decisions made and the reasons for them
- files created or modified, with exact paths
- key commands run, code written, and their results
- unresolved errors and open questions
- concrete next steps

Be concise, factual, and specific. Never invent details. Output only the summary, no preamble.`

// CompactNow summarizes older messages on demand (the /compact command).
func (a *Agent) CompactNow(ctx context.Context, sess *session.Session) (int, error) {
	if sess == nil {
		return 0, fmt.Errorf("nil session")
	}
	if len(sess.Messages) <= 8 {
		return 0, fmt.Errorf("conversation is too short to compact")
	}
	before := len(sess.Messages)
	a.compact(ctx, &sess.Messages, sess, 6, llm.EstimatedTokens(sess.Messages))
	a.save(sess)
	removed := before - len(sess.Messages)
	if removed <= 0 {
		return 0, fmt.Errorf("nothing was compacted")
	}
	return removed, nil
}

func (a *Agent) maybeCompact(ctx context.Context, msgs *[]llm.Message, system string, sess *session.Session) {
	estimate := llm.EstimatedTokens(*msgs) + len(system)/4 + 2500
	threshold := int(float64(a.cfg.ContextWindow) * a.cfg.CompactAt)
	if estimate < threshold || len(*msgs) < 10 {
		return
	}
	a.compact(ctx, msgs, sess, 8, estimate)
}

// compact replaces older messages with a summary. Retried with a smaller tail
// when the provider rejects a request for exceeding the context window.
func (a *Agent) compact(ctx context.Context, msgs *[]llm.Message, sess *session.Session, keepLast int, before int) {
	if len(*msgs) <= keepLast+2 {
		return
	}
	cut := len(*msgs) - keepLast
	for cut > 1 && (*msgs)[cut].Role == "tool" {
		cut--
	}
	if cut <= 1 {
		return
	}
	old := (*msgs)[:cut]
	kept := append([]llm.Message(nil), (*msgs)[cut:]...)

	summary, err := a.summarize(ctx, old)
	note := ""
	if err != nil {
		summary = "[Earlier conversation was omitted because the context window was full. Re-read relevant files before continuing.]"
		note = " (summary failed: " + util.Truncate(err.Error(), 160) + ")"
	}

	summaryMsg := llm.Message{
		Role:    "user",
		Content: "[Conversation summary — earlier turns were compacted]\n\n" + summary + "\n\n[End of summary. Continue from here.]",
	}
	*msgs = append([]llm.Message{summaryMsg}, kept...)

	after := llm.EstimatedTokens(*msgs)
	a.emit.Emit(Event{
		Type:      EventCompact,
		SessionID: sessionID(sess),
		Depth:     a.depth,
		Message:   fmt.Sprintf("Compacted %d earlier messages into a summary (about %d → %d tokens)%s", len(old), before, after, note),
	})
	a.save(sess)
}

func (a *Agent) summarize(ctx context.Context, msgs []llm.Message) (string, error) {
	transcript := renderTranscript(msgs, 140000)
	if strings.TrimSpace(transcript) == "" {
		return "", fmt.Errorf("nothing to summarize")
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	resp, err := a.client.Complete(cctx, llm.ChatRequest{
		Model: a.cfg.Model,
		Messages: []llm.Message{
			{Role: "system", Content: summarizerPrompt},
			{Role: "user", Content: "Summarize this conversation:\n\n" + transcript},
		},
		MaxTokens:   1600,
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	a.usage = a.usage.Add(resp.Usage)
	return strings.TrimSpace(resp.Message.Content), nil
}

func renderTranscript(msgs []llm.Message, limit int) string {
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "user":
			fmt.Fprintf(&b, "\n[USER]\n%s\n", util.Truncate(m.Content, 8000))
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&b, "\n[ASSISTANT]\n%s\n", util.Truncate(m.Content, 8000))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "\n[TOOL CALL] %s(%s)\n", tc.Function.Name, util.Truncate(tc.Function.Arguments, 2000))
			}
		case "tool":
			fmt.Fprintf(&b, "\n[TOOL RESULT]\n%s\n", util.Truncate(m.Content, 5000))
		}
		if b.Len() > limit {
			s := b.String()
			head := limit / 4
			tail := limit - head
			return s[:head] + "\n…[transcript truncated]…\n" + s[len(s)-tail:]
		}
	}
	return b.String()
}

func isContextError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context length") ||
		strings.Contains(msg, "context window") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "maximum context") ||
		strings.Contains(msg, "exceeds the maximum")
}
