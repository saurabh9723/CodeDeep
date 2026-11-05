package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"codedeep/internal/tools"
	"codedeep/internal/util"
)

// RegisterTools exposes job management to the model.
func RegisterTools(r *tools.Registry, m *Manager) {
	if m == nil {
		return
	}
	r.Register(&tools.Spec{
		Name:        "job_list",
		Description: "List background shell jobs started in this session, including status and exit code.",
		ReadOnly:    true,
		Params:      tools.Object(map[string]any{}),
		Run: func(_ context.Context, _ json.RawMessage) (tools.Result, error) {
			list := m.List()
			if len(list) == 0 {
				return tools.Result{Output: "No background jobs."}, nil
			}
			var b strings.Builder
			for _, j := range list {
				fmt.Fprintf(&b, "%s  %-8s exit=%d pid=%d  %s\n", j.ID, j.Status, j.ExitCode, j.PID, util.Truncate(j.Command, 120))
			}
			return tools.Result{Output: strings.TrimRight(b.String(), "\n")}, nil
		},
	})

	r.Register(&tools.Spec{
		Name: "job_output",
		Description: "Read the output of a background job. Returns the job status, exit code, and the last " +
			"part of its combined stdout/stderr.",
		ReadOnly: true,
		Params: tools.Object(map[string]any{
			"id":        tools.StrProp("Job id, for example \"job-1\"."),
			"max_chars": tools.IntProp("Maximum output characters to return (default 12000).", 12000),
		}, "id"),
		Run: func(_ context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				ID       string `json:"id"`
				MaxChars int    `json:"max_chars"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return tools.Result{}, err
			}
			job, ok := m.Get(strings.TrimSpace(a.ID))
			if !ok {
				return tools.Result{Output: "Unknown job " + a.ID + ". Use job_list.", IsError: true}, nil
			}
			snap := job.Snapshot()
			limit := a.MaxChars
			if limit <= 0 {
				limit = 12000
			}
			if limit > 64*1024 {
				limit = 64 * 1024
			}
			text := snap.Output
			if len(text) > limit {
				text = text[len(text)-limit:]
				text = "…[output trimmed to last " + fmt.Sprint(limit) + " chars]\n" + text
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s · %s · exit %d · pid %d\n$ %s\n", snap.ID, snap.Status, snap.ExitCode, snap.PID, snap.Command)
			if strings.TrimSpace(text) == "" {
				b.WriteString("(no output yet)")
			} else {
				b.WriteString(text)
			}
			if snap.Truncated {
				b.WriteString("\n[older output was discarded]")
			}
			return tools.Result{Output: b.String()}, nil
		},
	})

	r.Register(&tools.Spec{
		Name:        "job_kill",
		Description: "Stop a running background job and its child processes.",
		Params: tools.Object(map[string]any{
			"id": tools.StrProp("Job id from job_list."),
		}, "id"),
		Run: func(_ context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return tools.Result{}, err
			}
			job, ok := m.Get(strings.TrimSpace(a.ID))
			if !ok {
				return tools.Result{Output: "Unknown job " + a.ID + ". Use job_list.", IsError: true}, nil
			}
			if err := job.Kill(); err != nil {
				return tools.Result{Output: err.Error(), IsError: true}, nil
			}
			job.Wait(2 * time.Second)
			snap := job.Snapshot()
			return tools.Result{Output: fmt.Sprintf("Stopped %s (status %s, exit %d).", snap.ID, snap.Status, snap.ExitCode)}, nil
		},
	})
}
