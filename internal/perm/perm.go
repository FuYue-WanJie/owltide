package perm

// Permission service (P2b-M13): one in-memory table for every
// "waiting on the user" state (approvals, ask_user, plan approval,
// memory_save). CLI, serve and a future TUI all answer through this
// table.
//
// State machine: pending -> approved | denied | timeout | cancelled.
// Terminal states are immutable. Multiple clients race to answer: the
// first answer wins, later ones observe the terminal state plus the
// answering client's identifier ("已处理"). Timeouts resolve as a
// denial (state=timeout) — never an approval. Session end cancels
// everything still pending.
//
// Every terminal transition emits one decisions event:
//
//	{event: "permission", id, kind, state, by, source}
//
// with no payload body. Payloads stay in memory only.
//
// M13 deliberately does NOT migrate ask_user or the guard confirmer;
// those follow in M14.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Kind classifies the request.
type Kind string

const (
	KindApproval     Kind = "approval"
	KindAskUser      Kind = "ask_user"
	KindPlanApproval Kind = "plan_approval"
	KindMemorySave   Kind = "memory_save"
)

// State is the request lifecycle. pending is the only non-terminal
// state.
type State string

const (
	StatePending   State = "pending"
	StateApproved  State = "approved"
	StateDenied    State = "denied"
	StateTimeout   State = "timeout"
	StateCancelled State = "cancelled"
)

// Terminal reports whether s is one of the terminal states.
func (s State) Terminal() bool {
	switch s {
	case StateApproved, StateDenied, StateTimeout, StateCancelled:
		return true
	}
	return false
}

// Source identifies where a request came from.
type Source struct {
	Agent   string `json:"agent"`   // "main" or a sub-agent id
	Role    string `json:"role"`    // role name (may be empty)
	Session string `json:"session"` // session id
}

// Request is one pending/answered item. Payload is type-specific
// (command argv, question text, plan id, memory body) and is never
// copied into the decisions event.
type Request struct {
	ID       string    `json:"id"`
	Kind     Kind      `json:"kind"`
	Source   Source    `json:"source"`
	Payload  any       `json:"payload,omitempty"`
	Created  time.Time `json:"created"`
	State    State     `json:"state"`
	Answered time.Time `json:"answered,omitempty"`
	By       string    `json:"by,omitempty"` // answering client; "timeout" / "session_end" for automatic terminals
}

// Outcome is what an answerer or waiter observes.
type Outcome struct {
	Request Request
	// Won is true only for the caller that published the terminal
	// state; false means the request was already handled (or the
	// caller simply observed a terminal state).
	Won bool
}

// EventFunc is the decisions sink (same shape as subagent events).
type EventFunc func(event string, fields map[string]any)

// DefaultTimeout is used when Options.Timeout is unset (config
// permissions.timeout_sec, default 300s).
const DefaultTimeout = 300 * time.Second

// Options configures a Service.
type Options struct {
	// Timeout bounds how long a request may stay pending; the expiry
	// resolves it as a denial with state=timeout. <=0 means
	// DefaultTimeout.
	Timeout time.Duration
	// Events receives one event per terminal transition; nil disables.
	Events EventFunc
	// Now overrides the clock (tests); nil means time.Now.
	Now func() time.Time
}

type entry struct {
	req   Request
	done  chan struct{}
	timer *time.Timer
}

// Service is the in-memory permission table.
type Service struct {
	mu      sync.Mutex
	seq     int
	reqs    map[string]*entry
	timeout time.Duration
	events  EventFunc
	now     func() time.Time
}

// New builds a Service.
func New(opt Options) *Service {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		reqs:    map[string]*entry{},
		timeout: timeout,
		events:  opt.Events,
		now:     now,
	}
}

// Timeout returns the configured pending timeout.
func (s *Service) Timeout() time.Duration { return s.timeout }

// Ask registers a pending request and arms its timeout.
func (s *Service) Ask(kind Kind, src Source, payload any) (Request, error) {
	if kind == "" {
		return Request{}, fmt.Errorf("permission: kind is required")
	}
	if src.Agent == "" {
		src.Agent = "main"
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("req-%d", s.seq)
	r := Request{
		ID:      id,
		Kind:    kind,
		Source:  src,
		Payload: payload,
		Created: s.now(),
		State:   StatePending,
	}
	e := &entry{req: r, done: make(chan struct{})}
	e.timer = time.AfterFunc(s.timeout, func() {
		_, _ = s.resolve(id, StateTimeout, "timeout")
	})
	s.reqs[id] = e
	s.mu.Unlock()
	return r, nil
}

// Answer resolves a pending request on behalf of one client. The
// first caller wins; later callers get Won=false plus the terminal
// state and the winner's identifier. state must be approved or denied.
func (s *Service) Answer(id string, state State, by string) (Outcome, error) {
	if state != StateApproved && state != StateDenied {
		return Outcome{}, fmt.Errorf("permission: answer state must be approved or denied, got %q", state)
	}
	if by == "" {
		by = "unknown"
	}
	return s.resolve(id, state, by)
}

// resolve publishes the terminal state if the request is still
// pending. It is the single place where states change, which makes
// terminal states immutable (first writer wins) and guarantees exactly
// one event per request.
func (s *Service) resolve(id string, state State, by string) (Outcome, error) {
	s.mu.Lock()
	e, ok := s.reqs[id]
	if !ok {
		s.mu.Unlock()
		return Outcome{}, fmt.Errorf("permission: unknown request %q", id)
	}
	if e.req.State != StatePending {
		out := Outcome{Request: e.req}
		s.mu.Unlock()
		return out, nil
	}
	e.req.State = state
	e.req.Answered = s.now()
	e.req.By = by
	if e.timer != nil {
		e.timer.Stop()
	}
	close(e.done)
	out := Outcome{Request: e.req, Won: true}
	fields := map[string]any{
		"id":     e.req.ID,
		"kind":   string(e.req.Kind),
		"state":  string(e.req.State),
		"by":     by,
		"source": e.req.Source.Agent,
	}
	s.mu.Unlock()

	if s.events != nil {
		s.events("permission", fields)
	}
	return out, nil
}

// Wait blocks until the request reaches a terminal state or ctx ends.
// A ctx cancellation does NOT resolve the request (session-end
// cancellation is explicit, see CancelSession).
func (s *Service) Wait(ctx context.Context, id string) (Outcome, error) {
	s.mu.Lock()
	e, ok := s.reqs[id]
	s.mu.Unlock()
	if !ok {
		return Outcome{}, fmt.Errorf("permission: unknown request %q", id)
	}
	select {
	case <-e.done:
		return s.Get(id)
	case <-ctx.Done():
		out, _ := s.Get(id)
		return out, ctx.Err()
	}
}

// Get returns a snapshot of one request.
func (s *Service) Get(id string) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.reqs[id]
	if !ok {
		return Outcome{}, fmt.Errorf("permission: unknown request %q", id)
	}
	return Outcome{Request: e.req}, nil
}

// Pending returns snapshots of every pending request (oldest first).
func (s *Service) Pending() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, 0, len(s.reqs))
	for _, e := range s.reqs {
		if e.req.State == StatePending {
			out = append(out, e.req)
		}
	}
	// seq order = creation order
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// CancelSession cancels every pending request of one session (empty
// session id cancels all pending: process/session teardown).
func (s *Service) CancelSession(session string) {
	s.mu.Lock()
	var ids []string
	for id, e := range s.reqs {
		if e.req.State != StatePending {
			continue
		}
		if session == "" || e.req.Source.Session == session {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		_, _ = s.resolve(id, StateCancelled, "session_end")
	}
}
