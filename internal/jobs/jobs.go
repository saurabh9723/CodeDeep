// Package jobs runs shell commands in the background and tracks their output.
package jobs

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"codedeep/internal/proc"
	"codedeep/internal/tools"
)

const (
	maxJobOutput = 256 * 1024
	maxJobs      = 32
)

// Snapshot is a stable view of a job for tools and the CLI.
type Snapshot struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Status    string    `json:"status"`
	ExitCode  int       `json:"exit_code"`
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitempty"`
	Output    string    `json:"output"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Job is one background command.
type Job struct {
	id      string
	command string
	status  string
	exit    int
	pid     int
	started time.Time
	ended   time.Time

	mu            sync.Mutex
	output        []byte
	truncated     bool
	killRequested bool
	cmd           *exec.Cmd
	done          chan struct{}
}

// Manager owns the jobs started by one CodeDeep process.
type Manager struct {
	mu      sync.Mutex
	shell   string
	workDir string
	seq     int64
	jobs    map[string]*Job
	order   []string
	closed  bool
}

func NewManager(shell, workDir string) *Manager {
	return &Manager{
		shell:   shell,
		workDir: workDir,
		jobs:    map[string]*Job{},
	}
}

// Start launches command through the configured shell and returns immediately.
func (m *Manager) Start(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("job command is required")
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", fmt.Errorf("job manager is closed")
	}
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)
	m.mu.Unlock()

	shell, args := tools.ShellCommand(m.shell, command)
	cmd := exec.Command(shell, args...)
	cmd.Dir = m.workDir
	cmd.Env = os.Environ()
	proc.Prepare(cmd)

	job := &Job{
		id:      id,
		command: command,
		status:  "running",
		started: time.Now().UTC(),
		done:    make(chan struct{}),
		cmd:     cmd,
	}
	cmd.Stdout = &jobWriter{job: job}
	cmd.Stderr = &jobWriter{job: job}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start job: %w", err)
	}
	job.pid = cmd.Process.Pid

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = proc.KillTree(cmd)
		return "", fmt.Errorf("job manager is closed")
	}
	m.jobs[id] = job
	m.order = append(m.order, id)
	for len(m.order) > maxJobs {
		oldest := m.order[0]
		if old := m.jobs[oldest]; old != nil && old.isRunning() {
			break
		}
		delete(m.jobs, oldest)
		m.order = m.order[1:]
	}
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		job.mu.Lock()
		job.ended = time.Now().UTC()
		job.exit = 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				job.exit = exitErr.ExitCode()
			} else {
				job.exit = -1
			}
		}
		if job.killRequested {
			job.status = "killed"
		} else {
			job.status = "exited"
		}
		job.mu.Unlock()
		close(job.done)
	}()

	return id, nil
}

func (m *Manager) Get(id string) (*Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	order := append([]string(nil), m.order...)
	jobs := make([]*Job, 0, len(order))
	for _, id := range order {
		if j := m.jobs[id]; j != nil {
			jobs = append(jobs, j)
		}
	}
	m.mu.Unlock()

	out := make([]Snapshot, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Snapshot())
	}
	return out
}

// Close stops every running job.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	var running []*Job
	for _, j := range m.jobs {
		if j.isRunning() {
			running = append(running, j)
		}
	}
	m.mu.Unlock()
	for _, j := range running {
		_ = j.Kill()
	}
}

func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	status := j.status
	exit := j.exit
	if status == "running" {
		exit = 0
	}
	return Snapshot{
		ID:        j.id,
		Command:   j.command,
		Status:    status,
		ExitCode:  exit,
		PID:       j.pid,
		Started:   j.started,
		Ended:     j.ended,
		Output:    string(j.output),
		Truncated: j.truncated,
	}
}

// Kill terminates the job's process tree.
func (j *Job) Kill() error {
	j.mu.Lock()
	if j.status != "running" {
		j.mu.Unlock()
		return fmt.Errorf("job %s is not running", j.id)
	}
	j.killRequested = true
	cmd := j.cmd
	j.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := proc.KillTree(cmd); err != nil {
		return fmt.Errorf("kill %s: %w", j.id, err)
	}
	return nil
}

func (j *Job) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.status == "running"
}

// Wait blocks until the job exits or the timeout elapses.
func (j *Job) Wait(timeout time.Duration) bool {
	if timeout <= 0 {
		<-j.done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-j.done:
		return true
	case <-timer.C:
		return false
	}
}

type jobWriter struct{ job *Job }

func (w *jobWriter) Write(p []byte) (int, error) {
	j := w.job
	j.mu.Lock()
	defer j.mu.Unlock()
	j.output = append(j.output, p...)
	if len(j.output) > maxJobOutput {
		drop := len(j.output) - maxJobOutput
		j.output = append([]byte(nil), j.output[drop:]...)
		j.truncated = true
	}
	return len(p), nil
}
