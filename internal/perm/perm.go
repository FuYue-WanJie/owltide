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
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	Agent   string `json:"agent"`             // "main" or a sub-agent id
	Role    string `json:"role"`              // role name (may be empty)
	Session string `json:"session"`           // session id
	Channel string `json:"channel,omitempty"` // social origin; empty = local
}

// Endpoint names for answers (audit "by" stays free-form).
const (
	EndpointCLI     = "cli"
	EndpointServe   = "serve"
	EndpointChannel = "channel"
)

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
	// Response is the textual answer for ask_user requests (set by
	// Respond; empty for yes/no approvals).
	Response string `json:"response,omitempty"`
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

// Retention defaults: at most DefaultRetain terminal requests are kept,
// and any terminal request older than DefaultRetainFor is dropped.
const (
	DefaultRetain    = 256
	DefaultRetainFor = 10 * time.Minute
)

// Errors for lookups of requests that no longer exist.
var (
	// ErrUnknown is returned for an id that was never issued.
	ErrUnknown = errors.New("permission: unknown request")
	// ErrExpired is returned for a request that reached a terminal
	// state and was removed by the retention policy.
	ErrExpired = errors.New("permission: request expired")
	// ErrRisky is returned when a session allow targets a command in
	// the non-overridable risk set.
	ErrRisky = errors.New("permission: command is in the non-overridable risk set")
	// ErrChannelReadOnly is returned for social-channel write attempts.
	ErrChannelReadOnly = errors.New("permission: social channel is read-only")
)

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
	// Retain caps how many terminal requests stay queryable; <=0 means
	// DefaultRetain.
	Retain int
	// RetainFor drops terminal requests older than this; <=0 means
	// DefaultRetainFor.
	RetainFor time.Duration
}

type entry struct {
	req   Request
	seq   int
	done  chan struct{}
	timer *time.Timer
}

// Service is the in-memory permission table.
type Service struct {
	mu        sync.Mutex
	seq       int
	reqs      map[string]*entry
	timeout   time.Duration
	retain    int
	retainFor time.Duration
	events    EventFunc
	now       func() time.Time
	// expired tombstones recently reaped ids so Get can say "expired"
	// instead of "unknown". Bounded (expiredMax) to stay small.
	expired   map[string]bool
	expiredAt []string
	// allows are the session-scoped, exact-argv command approvals
	// ("本会话允许"). In-memory only; cleared on session end.
	allows map[string]bool
}

// expiredMax caps the tombstone ring.
const expiredMax = 4096

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
	retain := opt.Retain
	if retain <= 0 {
		retain = DefaultRetain
	}
	retainFor := opt.RetainFor
	if retainFor <= 0 {
		retainFor = DefaultRetainFor
	}
	return &Service{
		reqs:      map[string]*entry{},
		timeout:   timeout,
		retain:    retain,
		retainFor: retainFor,
		events:    opt.Events,
		now:       now,
		expired:   map[string]bool{},
		allows:    map[string]bool{},
	}
}

// Timeout returns the configured pending timeout.
func (s *Service) Timeout() time.Duration { return s.timeout }

// Ask registers a pending request and arms its timeout.
func (s *Service) Ask(kind Kind, src Source, payload any) (Request, error) {
	if kind == "" {
		return Request{}, fmt.Errorf("permission: kind is required")
	}
	if src.Channel != "" && kind != KindAskUser {
		// Social entry is read-only: it may ask/answer plain questions
		// but never trigger a write-class approval path.
		s.emitBlocked("channel_write_request", map[string]any{
			"kind": string(kind), "source": src.Agent, "channel": src.Channel,
		})
		return Request{}, fmt.Errorf("%w: %s", ErrChannelReadOnly, kind)
	}
	if src.Agent == "" {
		src.Agent = "main"
	}
	s.mu.Lock()
	s.reapLocked()
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
	e := &entry{req: r, seq: s.seq, done: make(chan struct{})}
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

// Respond approves an ask_user request with its textual answer. It is
// the ask_user counterpart of Answer: first caller wins, later callers
// observe the existing terminal state (and its response).
func (s *Service) Respond(id string, response string, by string) (Outcome, error) {
	if by == "" {
		by = "unknown"
	}
	return s.resolveWith(id, StateApproved, by, response)
}

// resolve publishes the terminal state if the request is still
// pending. It is the single place where states change, which makes
// terminal states immutable (first writer wins) and guarantees exactly
// one event per request.
func (s *Service) resolve(id string, state State, by string) (Outcome, error) {
	return s.resolveWith(id, state, by, "")
}

func (s *Service) resolveWith(id string, state State, by, response string) (Outcome, error) {
	s.mu.Lock()
	e, err := s.lookupLocked(id)
	if err != nil {
		s.mu.Unlock()
		return Outcome{}, err
	}
	if e.req.State != StatePending {
		out := Outcome{Request: e.req}
		s.mu.Unlock()
		return out, nil
	}
	e.req.State = state
	e.req.Answered = s.now()
	e.req.By = by
	if response != "" {
		e.req.Response = response
	}
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

// emitBlocked records a refused social-channel attempt as a decisions
// event (reason/id/kind/source only, no payload).
func (s *Service) emitBlocked(reason string, fields map[string]any) {
	if s.events == nil {
		return
	}
	out := map[string]any{"reason": reason}
	for k, v := range fields {
		out[k] = v
	}
	s.events("permission_blocked", out)
}

// AnswerAs resolves a request on behalf of a named endpoint. The
// social-channel endpoint is read-only: it cannot answer approvals (or
// any other request), and the attempt is audited.
func (s *Service) AnswerAs(id string, state State, by, endpoint string) (Outcome, error) {
	if endpoint == EndpointChannel {
		s.emitBlocked("channel_answer", map[string]any{"id": id, "by": by, "kind": "answer"})
		return Outcome{}, ErrChannelReadOnly
	}
	return s.Answer(id, state, by)
}

// ---- session allows ("本会话允许") ----

// Allow records a session-scoped approval for one exact command
// signature (tool.CommandSignature). Risk-set commands are refused:
// they always require individual confirmation. Memory only; cleared on
// session end.
func (s *Service) Allow(sig string, risky bool) error {
	if sig == "" {
		return fmt.Errorf("permission: empty command signature")
	}
	if risky {
		return ErrRisky
	}
	s.mu.Lock()
	s.allows[sig] = true
	s.mu.Unlock()
	return nil
}

// CommandAllowed reports whether the exact signature is allowlisted.
func (s *Service) CommandAllowed(sig string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allows[sig]
}

// Allows lists the session-approved signatures (sorted, stable).
func (s *Service) Allows() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.allows))
	for sig := range s.allows {
		out = append(out, sig)
	}
	sort.Strings(out)
	return out
}

// Revoke removes one signature from the allowlist.
func (s *Service) Revoke(sig string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.allows[sig]
	delete(s.allows, sig)
	return ok
}

// RevokeAll clears the allowlist (session end).
func (s *Service) RevokeAll() {
	s.mu.Lock()
	s.allows = map[string]bool{}
	s.mu.Unlock()
}

// Wait blocks until the request reaches a terminal state or ctx ends.
// A ctx cancellation does NOT resolve the request (session-end
// cancellation is explicit, see CancelSession).
func (s *Service) Wait(ctx context.Context, id string) (Outcome, error) {
	s.mu.Lock()
	e, err := s.lookupLocked(id)
	s.mu.Unlock()
	if err != nil {
		return Outcome{}, err
	}
	select {
	case <-e.done:
		return s.Get(id)
	case <-ctx.Done():
		out, _ := s.Get(id)
		return out, ctx.Err()
	}
}

// Get returns a snapshot of one request. A request removed by the
// retention policy yields ErrExpired (distinct from ErrUnknown).
func (s *Service) Get(id string) (Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	e, ok := s.reqs[id]
	if !ok {
		if s.expired[id] {
			return Outcome{}, fmt.Errorf("%w: %q", ErrExpired, id)
		}
		return Outcome{}, fmt.Errorf("%w: %q", ErrUnknown, id)
	}
	return Outcome{Request: e.req}, nil
}

// lookupLocked classifies a missing id.
func (s *Service) lookupLocked(id string) (*entry, error) {
	if e, ok := s.reqs[id]; ok {
		return e, nil
	}
	if s.expired[id] {
		return nil, fmt.Errorf("%w: %q", ErrExpired, id)
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknown, id)
}

// reapLocked drops terminal requests per the retention policy: first
// those older than RetainFor, then the oldest beyond Retain. Pending
// requests are never dropped. Removed ids are remembered (bounded) so
// Get can report them as expired.
func (s *Service) reapLocked() {
	now := s.now()
	var terminal []*entry
	for id, e := range s.reqs {
		if e.req.State == StatePending {
			continue
		}
		if now.Sub(e.req.Answered) > s.retainFor {
			s.dropLocked(id)
			continue
		}
		terminal = append(terminal, e)
	}
	if len(terminal) > s.retain {
		sort.Slice(terminal, func(i, j int) bool { return terminal[i].seq < terminal[j].seq })
		for i := 0; i < len(terminal)-s.retain; i++ {
			s.dropLocked(terminal[i].req.ID)
		}
	}
}

func (s *Service) dropLocked(id string) {
	delete(s.reqs, id)
	if s.expired[id] {
		return
	}
	s.expired[id] = true
	s.expiredAt = append(s.expiredAt, id)
	if len(s.expiredAt) > expiredMax {
		old := s.expiredAt[0]
		s.expiredAt = s.expiredAt[1:]
		delete(s.expired, old)
	}
}

// seqOf parses the numeric part of a request id (tests/diagnostics).
func seqOf(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "req-"))
	if err != nil {
		return 0
	}
	return n
}

// Pending returns snapshots of every pending request, oldest first.
// Ordering is by numeric sequence (req-9 before req-10), not by string.
func (s *Service) Pending() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked()
	type pair struct {
		seq int
		req Request
	}
	ps := make([]pair, 0, len(s.reqs))
	for _, e := range s.reqs {
		if e.req.State == StatePending {
			ps = append(ps, pair{e.seq, e.req})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].seq < ps[j].seq })
	out := make([]Request, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.req)
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
	// Session end invalidates every session allow.
	s.RevokeAll()
}
