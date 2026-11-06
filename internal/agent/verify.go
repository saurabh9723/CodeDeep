package agent

import (
	"context"
	"fmt"
	"strings"

	"codedeep/internal/llm"
	"codedeep/internal/session"
	"codedeep/internal/util"
)

const maxVerifyAttempts = 2

func (a *Agent) resetVerification() {
	a.locks.verify.Lock()
	a.locks.turnChanged = false
	a.locks.turnVerified = false
	a.locks.verifyAttempts = 0
	a.locks.verify.Unlock()
}

func (a *Agent) markChanged() {
	a.locks.verify.Lock()
	a.locks.turnChanged = true
	a.locks.turnVerified = false
	a.locks.verify.Unlock()
}

// verifyTurn runs the configured verification command when a turn changed
// files and the model is about to finish. A failure is added to the
// conversation so the model gets one or two chances to fix it; when the
// attempts run out the turn fails instead of reporting success.
func (a *Agent) verifyTurn(ctx context.Context, msgs *[]llm.Message, sess *session.Session) (bool, error) {
	if a.depth != 0 || a.verifier == nil || !a.verifier.Enabled() {
		return false, nil
	}
	a.locks.verify.Lock()
	need := a.locks.turnChanged && !a.locks.turnVerified
	attempts := a.locks.verifyAttempts
	if need {
		if attempts >= maxVerifyAttempts {
			a.locks.verify.Unlock()
			return false, fmt.Errorf("automatic verification failed %d times; stopping the turn", attempts)
		}
		a.locks.verifyAttempts++
		attempts++
	}
	a.locks.verify.Unlock()
	if !need {
		return false, nil
	}

	command := a.verifier.Command()
	a.emitEvent(Event{Type: EventVerifyStart, SessionID: sessionID(sess), Depth: a.depth, Message: command})
	result := a.verifier.Run(ctx)
	if err := ctx.Err(); err != nil {
		return false, err
	}
	a.emitEvent(Event{
		Type:      EventVerify,
		SessionID: sessionID(sess),
		Depth:     a.depth,
		Name:      "verify",
		Message:   result.Command,
		Output:    util.TruncateMiddle(result.Output, 64*1024),
		IsError:   result.IsError,
	})
	if !result.IsError {
		a.locks.verify.Lock()
		a.locks.turnVerified = true
		a.locks.verify.Unlock()
		return false, nil
	}
	if attempts >= maxVerifyAttempts {
		return false, fmt.Errorf("automatic verification failed %d times: %s",
			attempts, util.Truncate(strings.TrimSpace(result.Output), 300))
	}
	note := fmt.Sprintf("[automatic verification failed]\n$ %s\n%s\n\n"+
		"Fix the failure, then finish the task again. Verification runs automatically when you stop calling tools.",
		result.Command, util.TruncateMiddle(result.Output, 8000))
	*msgs = append(*msgs, llm.Message{Role: "user", Content: note})
	a.save(sess)
	return true, nil
}
