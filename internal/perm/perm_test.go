package perm

// P2b-M13 permission service tests: first-come-first-served under
// concurrency (-race), timeout denial, session cancellation, terminal
// immutability, and the exact decisions event shape (no payload body).

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// eventRecorder captures decisions events.
type eventRecorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func (r *eventRecorder) sink(event string, fields map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fields)
}

func (r *eventRecorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any{}, r.events...)
}

func testSource(session string) Source {
	return Source{Agent: "main", Role: "", Session: session}
}

// TestFirstComeFirstServed: eight concurrent answers race; exactly one
// wins, the others observe the terminal state plus the winner's
// identifier, and exactly one decisions event is emitted.
func TestFirstComeFirstServed(t *testing.T) {
	rec := &eventRecorder{}
	svc := New(Options{Timeout: 2 * time.Second, Events: rec.sink})

	req, err := svc.Ask(KindApproval, testSource("s1"), map[string]string{"cmd": "rm -rf x"})
	if err != nil {
		t.Fatal(err)
	}
	if req.State != StatePending || req.ID == "" {
		t.Fatalf("ask produced %s/%q", req.State, req.ID)
	}

	const n = 8
	var wg sync.WaitGroup
	outcomes := make([]Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], _ = svc.Answer(req.ID, StateApproved, "client-"+string(rune('A'+i)))
		}(i)
	}
	wg.Wait()

	winners := 0
	var winnerBy string
	for _, o := range outcomes {
		if o.Won {
			winners++
			winnerBy = o.Request.By
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	for _, o := range outcomes {
		if o.Request.State != StateApproved {
			t.Fatalf("every caller must observe approved, got %s", o.Request.State)
		}
		if o.Request.By != winnerBy {
			t.Fatalf("every caller must observe the winner's identifier: %q vs %q", o.Request.By, winnerBy)
		}
	}
	// Exactly one decisions event, and it carries no payload body.
	evs := rec.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e["id"] != req.ID || e["kind"] != "approval" || e["state"] != "approved" {
		t.Fatalf("event fields wrong: %v", e)
	}
	if _, has := e["payload"]; has {
		t.Fatalf("event must not carry the payload: %v", e)
	}
	if e["source"] != "main" {
		t.Fatalf("event source = %v, want main", e["source"])
	}
}

// TestTimeoutDenies: expiry resolves the request as a denial
// (state=timeout), and the waiter observes it without blocking.
func TestTimeoutDenies(t *testing.T) {
	rec := &eventRecorder{}
	svc := New(Options{Timeout: 60 * time.Millisecond, Events: rec.sink})

	req, _ := svc.Ask(KindAskUser, testSource("s1"), "question?")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := svc.Wait(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Request.State != StateTimeout {
		t.Fatalf("state = %s, want timeout", out.Request.State)
	}
	if out.Request.By != "timeout" {
		t.Fatalf("by = %q, want timeout", out.Request.By)
	}
	if out.Won {
		t.Fatal("a waiter never wins")
	}

	evs := rec.all()
	if len(evs) != 1 || evs[0]["state"] != "timeout" || evs[0]["by"] != "timeout" {
		t.Fatalf("timeout event wrong: %v", evs)
	}
}

// TestTimeoutDoesNotDenyAfterManualAnswer: an answered request is
// immune to its own timeout firing late.
func TestTimeoutDoesNotDenyAfterManualAnswer(t *testing.T) {
	rec := &eventRecorder{}
	svc := New(Options{Timeout: 30 * time.Millisecond, Events: rec.sink})
	req, _ := svc.Ask(KindApproval, testSource("s1"), nil)
	out, err := svc.Answer(req.ID, StateApproved, "client-fast")
	if err != nil || !out.Won {
		t.Fatalf("answer must win: %v/%v", out.Won, err)
	}
	time.Sleep(80 * time.Millisecond) // let the timer fire
	out, _ = svc.Get(req.ID)
	if out.Request.State != StateApproved {
		t.Fatalf("late timer overrode the answer: %s", out.Request.State)
	}
	if len(rec.all()) != 1 {
		t.Fatalf("late timer emitted extra events: %d", len(rec.all()))
	}
}

// TestCancelSession: session end cancels that session's pending
// requests; other sessions stay pending.
func TestCancelSession(t *testing.T) {
	rec := &eventRecorder{}
	svc := New(Options{Timeout: 5 * time.Second, Events: rec.sink})

	r1, _ := svc.Ask(KindApproval, testSource("s1"), nil)
	r2, _ := svc.Ask(KindAskUser, testSource("s1"), nil)
	r3, _ := svc.Ask(KindApproval, testSource("s2"), nil)

	svc.CancelSession("s1")

	for _, id := range []string{r1.ID, r2.ID} {
		out, _ := svc.Get(id)
		if out.Request.State != StateCancelled || out.Request.By != "session_end" {
			t.Fatalf("%s = %s/%s, want cancelled/session_end", id, out.Request.State, out.Request.By)
		}
	}
	out, _ := svc.Get(r3.ID)
	if out.Request.State != StatePending {
		t.Fatalf("other session must stay pending, got %s", out.Request.State)
	}

	svc.CancelSession("") // teardown: everything still pending
	out, _ = svc.Get(r3.ID)
	if out.Request.State != StateCancelled {
		t.Fatalf("empty session must cancel all pending, got %s", out.Request.State)
	}
	if n := len(rec.all()); n != 3 {
		t.Fatalf("events = %d, want 3 (one per terminal)", n)
	}
}

// TestTerminalImmutable: once terminal, later answers cannot change
// the state; they only observe it.
func TestTerminalImmutable(t *testing.T) {
	rec := &eventRecorder{}
	svc := New(Options{Timeout: 5 * time.Second, Events: rec.sink})
	req, _ := svc.Ask(KindPlanApproval, testSource("s1"), nil)

	if _, err := svc.Answer(req.ID, StateApproved, "client-A"); err != nil {
		t.Fatal(err)
	}
	out, err := svc.Answer(req.ID, StateDenied, "client-B")
	if err != nil {
		t.Fatal(err)
	}
	if out.Won {
		t.Fatal("second answer must not win")
	}
	if out.Request.State != StateApproved || out.Request.By != "client-A" {
		t.Fatalf("terminal mutated: %s/%s", out.Request.State, out.Request.By)
	}
	if len(rec.all()) != 1 {
		t.Fatalf("terminal must emit exactly one event, got %d", len(rec.all()))
	}
}

// TestAnswerStateValidation: only approved/denied are valid answers.
func TestAnswerStateValidation(t *testing.T) {
	svc := New(Options{})
	req, _ := svc.Ask(KindApproval, testSource("s1"), nil)
	if _, err := svc.Answer(req.ID, StateTimeout, "client-x"); err == nil {
		t.Fatal("timeout is not a client answer state")
	}
	if _, err := svc.Answer(req.ID, StateCancelled, "client-x"); err == nil {
		t.Fatal("cancelled is not a client answer state")
	}
}

// TestWaitContextDoesNotResolve: a cancelled waiter returns its ctx
// error but leaves the request pending.
func TestWaitContextDoesNotResolve(t *testing.T) {
	svc := New(Options{Timeout: 5 * time.Second})
	req, _ := svc.Ask(KindMemorySave, testSource("s1"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Wait(ctx, req.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	out, _ := svc.Get(req.ID)
	if out.Request.State != StatePending {
		t.Fatalf("ctx cancel must not resolve, got %s", out.Request.State)
	}
}

// TestPendingOrderAndSourceDefault: Pending is oldest-first; an empty
// Source.Agent defaults to "main".
func TestPendingOrderAndSourceDefault(t *testing.T) {
	svc := New(Options{})
	req, _ := svc.Ask(KindApproval, Source{}, nil) // Agent defaults to main
	if req.Source.Agent != "main" {
		t.Fatalf("agent = %q, want main", req.Source.Agent)
	}
	svc.Ask(KindAskUser, Source{Agent: "main", Session: "s1"}, nil)
	svc.Ask(KindAskUser, Source{Agent: "main", Session: "s1"}, nil)
	p := svc.Pending()
	if len(p) != 3 {
		t.Fatalf("pending = %d, want 3", len(p))
	}
	for i := 1; i < len(p); i++ {
		if p[i].ID <= p[i-1].ID {
			t.Fatalf("pending not ordered: %v", p)
		}
	}
}

// TestDefaults: unset timeout uses the 300s default.
func TestDefaults(t *testing.T) {
	svc := New(Options{})
	if svc.Timeout() != DefaultTimeout {
		t.Fatalf("timeout = %v, want %v", svc.Timeout(), DefaultTimeout)
	}
	if svc.Timeout() != 300*time.Second {
		t.Fatalf("default timeout = %v, want 300s", svc.Timeout())
	}
}

// TestPendingNumericOrder: 12+ pending requests list as req-1..req-12
// (numeric order, not string order where req-10 < req-9).
func TestPendingNumericOrder(t *testing.T) {
	svc := New(Options{})
	for i := 0; i < 12; i++ {
		if _, err := svc.Ask(KindAskUser, testSource("s1"), nil); err != nil {
			t.Fatal(err)
		}
	}
	p := svc.Pending()
	if len(p) != 12 {
		t.Fatalf("pending = %d, want 12", len(p))
	}
	for i, r := range p {
		want := "req-" + strconv.Itoa(i+1)
		if r.ID != want {
			t.Fatalf("position %d = %s, want %s (numeric order)", i, r.ID, want)
		}
	}
}

// TestRetentionCap: only the most recent Retain terminal requests stay
// queryable; older ones report ErrExpired (not ErrUnknown).
func TestRetentionCap(t *testing.T) {
	rec := &eventRecorder{}
	svc := New(Options{Retain: 3, RetainFor: time.Hour, Events: rec.sink})
	var ids []string
	for i := 0; i < 5; i++ {
		r, err := svc.Ask(KindApproval, testSource("s1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
		if _, err := svc.Answer(r.ID, StateApproved, "cli"); err != nil {
			t.Fatal(err)
		}
	}
	// Get triggers the reap; the oldest two are gone.
	for _, id := range ids[:2] {
		if _, err := svc.Get(id); !errors.Is(err, ErrExpired) {
			t.Fatalf("Get(%s) = %v, want ErrExpired", id, err)
		}
	}
	for _, id := range ids[2:] {
		if _, err := svc.Get(id); err != nil {
			t.Fatalf("Get(%s) must still work: %v", id, err)
		}
	}
	// Never-issued ids stay ErrUnknown.
	if _, err := svc.Get("req-999"); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown id = %v, want ErrUnknown", err)
	}
}

// TestRetentionTTL: terminal requests older than RetainFor are dropped
// even when under the count cap.
func TestRetentionTTL(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	svc := New(Options{Retain: 100, RetainFor: 10 * time.Minute,
		Now: func() time.Time { return now }})
	r, _ := svc.Ask(KindApproval, testSource("s1"), nil)
	if _, err := svc.Answer(r.ID, StateApproved, "cli"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	if _, err := svc.Get(r.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired id = %v, want ErrExpired", err)
	}
}

// TestPendingNeverReaped: pending requests survive retention pressure.
func TestPendingNeverReaped(t *testing.T) {
	svc := New(Options{Retain: 1, RetainFor: time.Hour})
	pending, _ := svc.Ask(KindAskUser, testSource("s1"), nil)
	for i := 0; i < 4; i++ {
		r, _ := svc.Ask(KindApproval, testSource("s1"), nil)
		svc.Answer(r.ID, StateApproved, "cli")
	}
	if _, err := svc.Get(pending.ID); err != nil {
		t.Fatalf("pending request was reaped: %v", err)
	}
}
