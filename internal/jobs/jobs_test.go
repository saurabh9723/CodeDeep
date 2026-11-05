package jobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codedeep/internal/tools"
)

func cmdShell() bool {
	shell, _ := tools.ShellCommand("", "x")
	return strings.EqualFold(filepath.Base(shell), "cmd.exe")
}

func longCommand() string {
	if cmdShell() {
		return "ping -n 31 127.0.0.1 >nul"
	}
	return "sleep 30"
}

func TestStartCapturesOutput(t *testing.T) {
	m := NewManager("", t.TempDir())
	defer m.Close()

	id, err := m.Start("echo hello-job")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "job-1" {
		t.Errorf("id = %q", id)
	}
	job, ok := m.Get(id)
	if !ok {
		t.Fatal("job not found")
	}
	if !job.Wait(10 * time.Second) {
		t.Fatal("job did not finish")
	}
	snap := job.Snapshot()
	if snap.Status != "exited" || snap.ExitCode != 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !strings.Contains(snap.Output, "hello-job") {
		t.Fatalf("output = %q", snap.Output)
	}
	if snap.PID <= 0 || snap.Ended.IsZero() {
		t.Fatalf("snapshot = %+v", snap)
	}

	list := m.List()
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v", list)
	}
}

func TestFailureExitCode(t *testing.T) {
	m := NewManager("", t.TempDir())
	defer m.Close()
	id, err := m.Start("exit 4")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, _ := m.Get(id)
	if !job.Wait(10 * time.Second) {
		t.Fatal("job did not finish")
	}
	if snap := job.Snapshot(); snap.Status != "exited" || snap.ExitCode != 4 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestKillRunningJob(t *testing.T) {
	m := NewManager("", t.TempDir())
	defer m.Close()
	id, err := m.Start(longCommand())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, _ := m.Get(id)
	if err := job.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !job.Wait(10 * time.Second) {
		t.Fatal("killed job did not exit")
	}
	if snap := job.Snapshot(); snap.Status != "killed" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if err := job.Kill(); err == nil {
		t.Fatal("killing a finished job should fail")
	}
}

func TestManagerCloseStopsJobs(t *testing.T) {
	m := NewManager("", t.TempDir())
	id, err := m.Start(longCommand())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, _ := m.Get(id)
	m.Close()
	if !job.Wait(10 * time.Second) {
		t.Fatal("Close did not stop the job")
	}
	if _, err := m.Start("echo late"); err == nil {
		t.Fatal("Start after Close should fail")
	}
}

func TestJobTools(t *testing.T) {
	m := NewManager("", t.TempDir())
	defer m.Close()
	r := tools.NewRegistry()
	RegisterTools(r, m)

	id, err := m.Start("echo tool-output")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	job, _ := m.Get(id)
	job.Wait(10 * time.Second)

	outputSpec, ok := r.Get("job_output")
	if !ok {
		t.Fatal("job_output not registered")
	}
	res, err := outputSpec.Run(context.Background(), json.RawMessage(`{"id":"job-1"}`))
	if err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if res.IsError || !strings.Contains(res.Output, "tool-output") || !strings.Contains(res.Output, "exited") {
		t.Fatalf("job_output = %+v", res)
	}

	listSpec, _ := r.Get("job_list")
	res, err = listSpec.Run(context.Background(), nil)
	if err != nil || !strings.Contains(res.Output, "job-1") {
		t.Fatalf("job_list = %+v, %v", res, err)
	}

	killSpec, _ := r.Get("job_kill")
	res, err = killSpec.Run(context.Background(), json.RawMessage(`{"id":"job-1"}`))
	if err != nil {
		t.Fatalf("job_kill: %v", err)
	}
	if !res.IsError {
		t.Fatalf("killing a finished job should report an error: %+v", res)
	}
}
