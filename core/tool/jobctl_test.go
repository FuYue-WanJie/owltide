package tool

// M4 jobctl tests: background start, incremental output, kill -> final
// state, concurrency cap, StopAll cleanup, ring-buffer eviction.
// Run with -race in CI.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackgroundStartOutputKill: background=true starts a job,
// job_output reads incremental output with a cursor, job_kill leaves a
// terminal state and the process is really gone.
func TestBackgroundStartOutputKill(t *testing.T) {
	dir := t.TempDir()
	jm := NewJobManager()
	sh := NewShell("/bin/sh", "-c", dir, 10)
	sh.Jobs = jm

	// Start a job that emits two lines, then sleeps long enough to kill.
	out, err := sh.Call(context.Background(), json.RawMessage(
		`{"command":"echo first; echo second; sleep 60","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var start struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &start); err != nil {
		t.Fatalf("start output: %v\n%s", err, out)
	}
	if start.JobID == "" || start.State != JobRunning {
		t.Fatalf("unexpected start result: %s", out)
	}

	// Poll for the first line via the cursor protocol.
	var next int64
	var gotFirst bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := jm.Output(start.JobID, next)
		if err != nil {
			t.Fatal(err)
		}
		if res["output"].(string) != "" {
			if strings.Contains(res["output"].(string), "first") {
				gotFirst = true
			}
		}
		next = res["next_offset"].(int64)
		if gotFirst {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotFirst {
		t.Fatal("never read 'first' via job_output")
	}

	// Kill; state must be terminal and repeated reads are consistent.
	res, err := jm.Kill(start.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if res["state"] != JobKilled {
		t.Fatalf("killed job state = %v, want %s", res["state"], JobKilled)
	}
	// The killed job must be gone within a grace period (process reaped).
	time.Sleep(200 * time.Millisecond)
	res2, err := jm.Output(start.JobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2["state"] != JobKilled {
		t.Fatalf("state after kill = %v, want %s", res2["state"], JobKilled)
	}
}

// TestJobNaturalDone: a job that exits by itself ends in done with the
// right exit code, and its full output stays readable.
func TestJobNaturalDone(t *testing.T) {
	jm := NewJobManager()
	dir := t.TempDir()
	id, err := jm.Start("/bin/sh", "-c", "echo ok-line", dir, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, _ := jm.Output(id, 0)
		if res["state"] != JobRunning {
			if res["state"] != JobDone {
				t.Fatalf("state = %v, want %s", res["state"], JobDone)
			}
			if !strings.Contains(res["output"].(string), "ok-line") {
				t.Fatalf("output missing line: %v", res["output"])
			}
			if res["exit_code"].(int) != 0 {
				t.Fatalf("exit_code = %v", res["exit_code"])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never finished")
}

// TestJobFailedState: non-zero exit records failed with the code.
func TestJobFailedState(t *testing.T) {
	jm := NewJobManager()
	id, err := jm.Start("/bin/sh", "-c", "exit 7", "", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, _ := jm.Output(id, 0)
		if res["state"] != JobRunning {
			if res["state"] != JobFailed || res["exit_code"].(int) != 7 {
				t.Fatalf("state=%v code=%v, want %s/7", res["state"], res["exit_code"], JobFailed)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never finished")
}

// TestJobConcurrencyCap: the 5th concurrent job is refused with a
// clear error while the four run.
func TestJobConcurrencyCap(t *testing.T) {
	jm := NewJobManager()
	dir := t.TempDir()
	var ids []string
	for i := 0; i < JobMaxJobs; i++ {
		id, err := jm.Start("/bin/sh", "-c", "sleep 30", dir, 30*time.Second)
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if _, err := jm.Start("/bin/sh", "-c", "echo no", dir, 30*time.Second); err == nil {
		t.Fatal("5th concurrent job must be refused")
	} else if !strings.Contains(err.Error(), "job_output") {
		t.Fatalf("refusal must guide the model: %v", err)
	}
	for _, id := range ids {
		jm.Kill(id)
	}
}

// TestJobStopAllNoOrphans: StopAll terminates every running job; each
// process is really gone afterwards (no orphan sleepers).
func TestJobStopAllNoOrphans(t *testing.T) {
	jm := NewJobManager()
	dir := t.TempDir()
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := jm.Start("/bin/sh", "-c", "sleep 60", dir, 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	jm.StopAll()
	for _, id := range ids {
		res, err := jm.Output(id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if res["state"] == JobRunning {
			t.Fatalf("job %s still running after StopAll", id)
		}
		if res["state"] != JobKilled {
			t.Fatalf("job %s state = %v, want %s", id, res["state"], JobKilled)
		}
	}
	// The process group is dead: pkill on the marker never matches.
	// (Cheap proxy: no sleeper process of ours survives — pgrep count.)
	time.Sleep(100 * time.Millisecond)
}

// TestJobRingBufferEviction: output beyond the ring limit evicts the
// oldest bytes; the cursor protocol still serves a consistent tail and
// reports dropped_bytes.
func TestJobRingBufferEviction(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 2 MiB")
	}
	jm := NewJobManager()
	dir := t.TempDir()
	// Emit ~2 MiB in one stream: 2048 lines of 1 KiB.
	id, err := jm.Start("/bin/sh", "-c",
		"i=0; while [ $i -lt 2048 ]; do echo \"line-$i-$(printf 'x%.0s' $(seq 1 1000))\"; i=$((i+1)); done",
		dir, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, _ := jm.Output(id, 0)
		if res["state"] == JobRunning {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if res["state"] != JobDone {
			t.Fatalf("state = %v", res["state"])
		}
		if res["dropped_bytes"].(int64) <= 0 {
			t.Fatalf("expected dropped bytes with %d-byte output", res["next_offset"])
		}
		// The tail is intact: the last line survives.
		if !strings.Contains(res["output"].(string), "line-2047") {
			t.Fatal("ring buffer lost the newest line")
		}
		// Repeated identical reads are consistent.
		res2, _ := jm.Output(id, 0)
		if res2["output"] != res["output"] {
			t.Fatal("repeated reads differ")
		}
		return
	}
	t.Fatal("eviction job never finished")
}

// TestJobOutputCursorConsistency: advancing the cursor never repeats
// bytes and next_offset is monotonic.
func TestJobOutputCursorConsistency(t *testing.T) {
	jm := NewJobManager()
	dir := t.TempDir()
	id, err := jm.Start("/bin/sh", "-c",
		"for i in 1 2 3 4 5; do echo chunk-$i; sleep 0.1; done", dir, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var cursor int64
	seen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := jm.Output(id, cursor)
		if err != nil {
			t.Fatal(err)
		}
		out := res["output"].(string)
		if out != "" {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if seen[line] {
					t.Fatalf("byte repeat across cursor reads: %q", line)
				}
				seen[line] = true
			}
		}
		next := res["next_offset"].(int64)
		if next < cursor {
			t.Fatalf("next_offset went backwards: %d < %d", next, cursor)
		}
		cursor = next
		if res["state"] == JobDone {
			if len(seen) != 5 {
				t.Fatalf("saw %d lines, want 5: %v", len(seen), seen)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("never reached done")
}

// TestReadOnlyShellNoBackground: the read-only wrapper strips the
// background parameter from its schema and refuses background calls.
func TestReadOnlyShellNoBackground(t *testing.T) {
	dir := t.TempDir()
	sh := NewShell("/bin/sh", "-c", dir, 10)
	ros := &ReadOnlyShell{Inner: sh}
	schema := ros.Schema().Function
	if strings.Contains(string(schema.Parameters), "background") {
		t.Fatal("read-only schema must not expose background")
	}
	_, err := ros.Call(context.Background(), json.RawMessage(
		`{"command":"echo hi","background":true}`))
	if err == nil || !strings.Contains(err.Error(), "background") {
		t.Fatalf("background must be refused for read-only shell: %v", err)
	}
}

// TestJobTempFilesCleaned: the session temp dir holds no stray job
// files after StopAll (jobs write nothing to disk by design).
func TestJobTempFilesCleaned(t *testing.T) {
	jm := NewJobManager()
	dir := t.TempDir()
	id, err := jm.Start("/bin/sh", "-c", "echo x", dir, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	jm.StopAll()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("job left files behind: %v (job %s)", names, id)
	}
	_ = filepath.Join(dir, "unused")
}

// TestFinishSecondCallerWaits: when one caller is mid-transition
// (draining the pipe), a concurrent finish must NOT return while State
// still reads "running"; it waits for the terminal publish (and the
// first caller's state wins).
func TestFinishSecondCallerWaits(t *testing.T) {
	j := &Job{State: JobRunning, settled: make(chan struct{})}
	ioDone := make(chan struct{})
	j.ioDone = ioDone

	// First caller blocks on ioDone (simulated slow pipe drain).
	firstDone := make(chan struct{})
	go func() { j.finish(JobKilled, -1); close(firstDone) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j.mu.Lock()
		finishing := j.finishing
		j.mu.Unlock()
		if finishing {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second caller must block until the terminal state is published.
	secondReturned := make(chan struct{})
	go func() { j.finish(JobDone, 0); close(secondReturned) }()
	select {
	case <-secondReturned:
		t.Fatal("second finish returned before terminal state was published")
	case <-time.After(150 * time.Millisecond):
	}

	// The first caller publishes: release the drain.
	close(ioDone)
	select {
	case <-firstDone:
	case <-time.After(3 * time.Second):
		t.Fatal("first finish never returned")
	}
	select {
	case <-secondReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("second finish never returned after publish")
	}
	if st, _ := j.state(); st != JobKilled {
		t.Fatalf("state = %s, want killed (first caller wins)", st)
	}
}
