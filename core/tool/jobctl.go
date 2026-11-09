package tool

// Background job control for run_shell's background=true mode.
//
// A job is one shell command running in its own process group (Unix;
// plain process on Windows). Output is captured in a bounded buffer:
// the newest 1 MiB is kept, evicted bytes are counted and reported to
// the model through job_output. Jobs have a hard timeout; a job killed
// by it ends in state "timeout".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const (
	// JobMaxJobs caps concurrently running background jobs.
	JobMaxJobs = 4
	// JobBufLimit caps the captured output per job; older bytes are
	// dropped (counted in dropped).
	JobBufLimit = 1 << 20 // 1 MiB
	// JobTimeout is how long a job may run before it is killed.
	JobTimeout = 30 * time.Minute
	// JobKeepFinished: finished jobs stay queryable for this long.
	JobKeepFinished = 10 * time.Minute
)

// Job states: running -> done | failed | timed_out | killed.
const (
	JobRunning  = "running"
	JobDone     = "done"
	JobFailed   = "failed"
	JobTimedOut = "timed_out"
	JobKilled   = "killed"
)

// Job is one background shell command.
type Job struct {
	ID       string
	Command  string
	State    string
	ExitCode int // meaningful when State != JobRunning
	Started  time.Time
	Ended    time.Time

	mu       sync.Mutex
	buf      bytes.Buffer // ring: evicted head bytes are counted
	dropped  int64
	cursor   int64 // total bytes ever written (monotonic)
	cmd      *exec.Cmd
	cancel   func()
	killFunc func(*exec.Cmd) // platform-specific group kill
}

// snapshot is a consistent view of a job's output tail.
type snapshot struct {
	data    []byte
	dropped int64
	// startOffset is the absolute byte offset of data[0] in the full
	// (unbounded) output stream; data covers [startOffset, cursor).
	startOffset int64
	cursor      int64
	state       string
	exitCode    int
}

func (j *Job) appendOutput(p []byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf.Write(p)
	j.cursor += int64(len(p))
	if over := j.buf.Len() - JobBufLimit; over > 0 {
		j.buf.Next(over) // drop the oldest bytes
		j.dropped += int64(over)
	}
}

func (j *Job) snap(since int64) snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	bufBytes := j.buf.Bytes()
	startOffset := j.cursor - int64(len(bufBytes))
	var data []byte
	if since < startOffset {
		// The requested window was partly evicted: serve what remains.
		data = append([]byte(nil), bufBytes...)
	} else {
		data = append([]byte(nil), bufBytes[since-startOffset:]...)
	}
	return snapshot{
		data: data, dropped: j.dropped,
		startOffset: startOffset, cursor: j.cursor,
		state: j.State, exitCode: j.ExitCode,
	}
}

func (j *Job) finish(state string, code int) {
	j.mu.Lock()
	if j.State != JobRunning {
		j.mu.Unlock()
		return
	}
	j.State = state
	j.ExitCode = code
	j.Ended = time.Now()
	cmd, kill := j.cmd, j.killFunc
	j.mu.Unlock()
	if cmd != nil && kill != nil {
		kill(cmd)
	}
}

// JobManager owns all background jobs of one session.
type JobManager struct {
	mu    sync.Mutex
	jobs  map[string]*Job
	seq   int
	clock func() time.Time
}

// NewJobManager builds a manager; used by the main registry wiring.
func NewJobManager() *JobManager {
	return &JobManager{jobs: map[string]*Job{}, clock: time.Now}
}

// runningCount counts jobs in the running state.
func (m *JobManager) runningCount() int {
	n := 0
	for _, j := range m.jobs {
		if j.State == JobRunning {
			n++
		}
	}
	return n
}

// reapFinished drops finished jobs older than JobKeepFinished so the
// map cannot grow without bound.
func (m *JobManager) reapFinished() {
	cutoff := m.clock().Add(-JobKeepFinished)
	for id, j := range m.jobs {
		if j.State != JobRunning && j.Ended.Before(cutoff) {
			delete(m.jobs, id)
		}
	}
}

// Start launches one background job; it enforces the concurrency cap
// and starts the platform process group. The returned id feeds
// job_output / job_kill.
func (m *JobManager) Start(bin, flag, command, dir string, timeout time.Duration) (string, error) {
	m.mu.Lock()
	m.reapFinished()
	if m.runningCount() >= JobMaxJobs {
		m.mu.Unlock()
		return "", fmt.Errorf("too many background jobs (%d running, limit %d); collect with job_output or stop with job_kill first",
			m.runningCount(), JobMaxJobs)
	}
	m.seq++
	id := fmt.Sprintf("job-%d", m.seq)
	j := &Job{ID: id, Command: command, State: JobRunning, Started: m.clock()}
	m.jobs[id] = j
	m.mu.Unlock()

	cmd := exec.Command(bin, flag, command)
	cmd.Dir = dir
	// Platform setup: setpgid on Unix so the whole tree dies together;
	// Windows needs no group (at least compiles and kills the main
	// process). The pipe must exist before the goroutine reads it.
	setJobProcessGroup(cmd)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		j.finish(JobFailed, -1)
		return "", fmt.Errorf("job pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // same pipe: combined stream
	if err := cmd.Start(); err != nil {
		j.finish(JobFailed, -1)
		return "", fmt.Errorf("job start: %w", err)
	}
	j.mu.Lock()
	j.cmd = cmd
	j.killFunc = killJobProcessGroup
	j.mu.Unlock()

	go func() {
		defer pipe.Close()
		buf := make([]byte, 32<<10)
		for {
			n, err := pipe.Read(buf)
			if n > 0 {
				j.appendOutput(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	// Watchdog: timeout kill + final state on process exit. All state
	// transitions go through finish (first writer wins, inside j.mu),
	// so a concurrent Kill cannot race this read.
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-timer.C:
			j.finish(JobTimedOut, -1)
			<-done
		case waitErr := <-done:
			if code, ok := exitCode(waitErr); ok && code == 0 {
				j.finish(JobDone, 0)
			} else if ok {
				j.finish(JobFailed, code)
			} else {
				j.finish(JobFailed, -1)
			}
		}
	}()
	return id, nil
}

// Output returns the incremental view of one job for job_output.
func (m *JobManager) Output(id string, since int64) (map[string]any, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job %q (finished jobs are kept for %v)", id, JobKeepFinished)
	}
	s := j.snap(since)
	if s.dropped > 0 && since < s.startOffset {
		// Part of the requested window was evicted.
		return map[string]any{
			"id": id, "state": s.state, "exit_code": s.exitCode,
			"output":        string(s.data),
			"next_offset":   s.cursor,
			"dropped_bytes": s.dropped,
			"note":          "output exceeded the ring buffer; the earliest bytes were dropped (count in dropped_bytes)",
		}, nil
	}
	return map[string]any{
		"id": id, "state": s.state, "exit_code": s.exitCode,
		"output": string(s.data), "next_offset": s.cursor,
	}, nil
}

// Kill stops one job and its process group; idempotent.
func (m *JobManager) Kill(id string) (map[string]any, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job %q", id)
	}
	if j.State == JobRunning {
		j.finish(JobKilled, -1)
	}
	return map[string]any{"id": id, "state": j.State, "killed": true}, nil
}

// StopAll finishes every running job (session end / run abort). It
// must not leave orphan processes behind.
func (m *JobManager) StopAll() {
	m.mu.Lock()
	var targets []*Job
	for _, j := range m.jobs {
		if j.State == JobRunning {
			targets = append(targets, j)
		}
	}
	m.mu.Unlock()
	for _, j := range targets {
		j.finish(JobKilled, -1)
	}
}

// ---- job_output / job_kill tools ----

// JobOutput reads a background job's incremental output.
type JobOutput struct{ M *JobManager }

func (JobOutput) Caps() Caps { return CapExec }

func (JobOutput) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name:        "job_output",
			Description: "Read a background job's new output since the given offset. Returns state, exit code, output and next_offset; repeat with next_offset until state is terminal.",
			Parameters: object(
				`"job_id":{"type":"string"},`+
					`"since_offset":{"type":"integer","description":"Byte offset returned as next_offset by the previous call; 0 starts from the beginning."}`,
				"job_id"),
		},
	}
}

func (t JobOutput) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		JobID       string `json:"job_id"`
		SinceOffset int64  `json:"since_offset"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.JobID == "" {
		return "", fmt.Errorf("job_id is required")
	}
	out, err := t.M.Output(a.JobID, a.SinceOffset)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// JobKill stops a background job and its process group.
type JobKill struct{ M *JobManager }

func (JobKill) Caps() Caps { return CapExec }

func (JobKill) Schema() Definition {
	return Definition{
		Type: "function",
		Function: FunctionSchema{
			Name:        "job_kill",
			Description: "Stop a background job (its whole process group). Idempotent; a killed job is terminal.",
			Parameters: object(
				`"job_id":{"type":"string"}`,
				"job_id"),
		},
	}
}

func (t JobKill) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if a.JobID == "" {
		return "", fmt.Errorf("job_id is required")
	}
	out, err := t.M.Kill(a.JobID)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

// Snapshot returns a status list of all known jobs (serve API view).
func (m *JobManager) Snapshot() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0, len(m.jobs))
	for id, j := range m.jobs {
		out = append(out, map[string]any{
			"id": id, "state": j.State, "exit_code": j.ExitCode,
			"command":  j.Command,
			"started":  j.Started.UTC().Format(time.RFC3339),
			"dropped":  j.dropped,
		})
	}
	return out
}
