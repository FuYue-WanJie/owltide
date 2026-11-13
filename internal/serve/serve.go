package serve

// Read-only HTTP API over the local runtime state: session list, one
// session's messages, sub-agent status, background job status, and an
// SSE stream pushing new messages and state changes. No write
// endpoints by design (the UI contract is not settled yet), and
// nothing secret ever leaves the process: API keys, endpoints and env
// contents are never loaded into the handler's reach.

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FuYue-WanJie/owltide/internal/perm"
)

//go:embed index.html
var indexHTML []byte

// Sessions abstracts the session-log directory (platform layout:
// <dir>/<session-id>/messages.jsonl), so tests can point it at a
// temp dir without touching the real home.
type Sessions struct {
	// Dir is the sessions root (e.g. ~/.owltide/sessions).
	Dir string
}

// SessionInfo is one entry of the session list.
type SessionInfo struct {
	ID       string `json:"id"`
	Messages int    `json:"messages"`
	Modified string `json:"modified"`
}

// List returns all sessions, newest first.
func (s Sessions) List() ([]SessionInfo, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out = make([]SessionInfo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue // stray files and hostile names are not sessions
		}
		info := SessionInfo{ID: e.Name()}
		if st, err := e.Info(); err == nil {
			info.Modified = st.ModTime().UTC().Format(time.RFC3339)
		}
		if n, err := countLines(filepath.Join(s.Dir, e.Name(), "messages.jsonl")); err == nil {
			info.Messages = n
		}
		out = append(out, info)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // newest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func countLines(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}

// Messages returns the raw message records of one session (each line
// of messages.jsonl parsed as JSON; torn lines are skipped).
func (s Sessions) Messages(id string) ([]json.RawMessage, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid session id")
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, id, "messages.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if json.Valid([]byte(line)) {
			out = append(out, json.RawMessage(line))
		}
	}
	return out, nil
}

// validID rejects path traversal: session ids are generated
// (YYYYMMDD-HHMMSS) but a hostile HTTP request may contain anything.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// State exposes live runtime state: sub-agent and job snapshots. The
// CLI wires it to narrow adapters so serve never imports subagent (no
// key/endpoint reach).
type State struct {
	mu     sync.Mutex
	subs   func() []map[string]any
	jobs   func() []map[string]any
	allows func() []string
}

// SetWiring installs the snapshot functions (called once at startup).
func (st *State) SetWiring(subs, jobs func() []map[string]any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.subs, st.jobs = subs, jobs
}

// SetAllows installs the session-allow provider (P2b-M15).
func (st *State) SetAllows(fn func() []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.allows = fn
}

// ---- HTTP handlers ----

// Server is the read-only API server.
type Server struct {
	Sessions Sessions
	State    *State
	// Perm is the live permission table (nil in a standalone serve:
	// approval endpoints then answer 503 and SSE has no approval
	// events).
	Perm PermSource
	// Token authenticates the answer endpoint (empty disables it).
	Token string
	hub   *hub
}

// New builds a server.
func New(s Sessions, st *State) *Server {
	return &Server{Sessions: s, State: st, hub: newHub()}
}

// Handler returns the full HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleSession)
	mux.HandleFunc("GET /api/subagents", s.handleSubs)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("GET /api/perm/allows", s.handleAllows)
	mux.HandleFunc("POST /api/permissions/{id}/answer", s.handlePermAnswer)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Sessions.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs, err := s.Sessions.Messages(id)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "no such session", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	writeJSON(w, msgs)
}

func (s *Server) handleSubs(w http.ResponseWriter, _ *http.Request) {
	s.State.mu.Lock()
	fn := s.State.subs
	s.State.mu.Unlock()
	list := make([]map[string]any, 0)
	if fn != nil {
		list = fn()
	}
	writeJSON(w, list)
}

func (s *Server) handleJobs(w http.ResponseWriter, _ *http.Request) {
	s.State.mu.Lock()
	fn := s.State.jobs
	s.State.mu.Unlock()
	list := make([]map[string]any, 0)
	if fn != nil {
		list = fn()
	}
	writeJSON(w, list)
}

// ---- SSE ----

// frame is one SSE event with its sequence number.
type frame struct {
	seq  int
	data []byte // full wire bytes: "id: <seq>\ndata: {...}\n\n"
}

const (
	sseRingSize  = 1024 // replay window
	sseSendBound = 5 * time.Second
)

// hub fans out SSE events with monotonic sequence numbers and a replay
// ring. Approval-class events are must-deliver: they are sent with
// backpressure (a client that stops draining for sseSendBound is
// dropped instead of hanging the publisher); progress-class events are
// lossy (non-blocking, drop-oldest is per-frame: newest wins on the
// channel).
type hub struct {
	mu      sync.Mutex
	seq     int
	ring    []frame
	clients map[chan []byte]bool
}

func newHub() *hub { return &hub{clients: map[chan []byte]bool{}} }

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 32)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *hub) drop(ch chan []byte) {
	h.unsubscribe(ch)
	close(ch)
}

// publish stores the event in the replay ring and delivers it.
func (h *hub) publish(event string, data any, mustDeliver bool) {
	h.mu.Lock()
	h.seq++
	n := h.seq
	payload, err := json.Marshal(map[string]any{"seq": n, "event": event, "data": data})
	if err != nil {
		h.mu.Unlock()
		return
	}
	f := frame{n, []byte(fmt.Sprintf("id: %d\ndata: %s\n\n", n, payload))}
	h.ring = append(h.ring, f)
	if len(h.ring) > sseRingSize {
		h.ring = h.ring[len(h.ring)-sseRingSize:]
	}
	clients := make([]chan []byte, 0, len(h.clients))
	for ch := range h.clients {
		clients = append(clients, ch)
	}
	h.mu.Unlock()

	for _, ch := range clients {
		if mustDeliver {
			select {
			case ch <- f.data:
			case <-time.After(sseSendBound):
				// Stalled client: drop the subscription rather than
				// hanging the publisher forever.
				h.drop(ch)
			}
			continue
		}
		select {
		case ch <- f.data:
		default: // slow progress client: drop the frame
		}
	}
}

// replay returns the frames after lastID; ok=false with reset=true
// means the client is behind the ring window and must refetch.
func (h *hub) replay(last int) (frames []frame, reset bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if last < 0 || len(h.ring) == 0 {
		return nil, false
	}
	if last+1 < h.ring[0].seq {
		return nil, true
	}
	for _, f := range h.ring {
		if f.seq > last {
			frames = append(frames, f)
		}
	}
	return frames, false
}

// snapshotFrame allocates a sequence number for a one-off frame that
// is NOT stored in the ring (e.g. the pending-approvals snapshot a new
// client receives on connect).
func (h *hub) snapshotFrame(event string, data any) frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	n := h.seq
	payload, err := json.Marshal(map[string]any{"seq": n, "event": event, "data": data})
	if err != nil {
		return frame{}
	}
	return frame{n, []byte(fmt.Sprintf("id: %d\ndata: %s\n\n", n, payload))}
}

// Publish pushes a lossy progress event into the SSE stream.
func (s *Server) Publish(event string, data any) { s.hub.publish(event, data, false) }

// PublishMust pushes a must-deliver event (approval class): all
// connected clients receive it; stalled clients are dropped rather
// than hanging the publisher.
func (s *Server) PublishMust(event string, data any) { s.hub.publish(event, data, true) }

// ---- permission wiring (P2b-M16) ----

// PermSource is the slice of the permission service the serve API
// needs. *perm.Service implements it.
type PermSource interface {
	Pending() []perm.Request
	AnswerAs(id string, state perm.State, by, endpoint string) (perm.Outcome, error)
	RespondAs(id, response, by, endpoint string) (perm.Outcome, error)
	Allows() []string
}

// pendingApprovals returns the current pending requests (nil without a
// live permission session).
func (s *Server) pendingApprovals() []perm.Request {
	if s.Perm == nil {
		return nil
	}
	return s.Perm.Pending()
}

func (s *Server) handleAllows(w http.ResponseWriter, _ *http.Request) {
	var list []string
	if s.Perm != nil {
		list = s.Perm.Allows()
	} else {
		s.State.mu.Lock()
		fn := s.State.allows
		s.State.mu.Unlock()
		if fn != nil {
			list = fn()
		}
	}
	if list == nil {
		list = []string{}
	}
	writeJSON(w, map[string]any{"allows": list})
}

// handlePermAnswer implements POST /api/permissions/{id}/answer (see
// serve-api.md): local token required, first come first served via the
// permission service.
func (s *Server) handlePermAnswer(w http.ResponseWriter, r *http.Request) {
	if s.Perm == nil {
		http.Error(w, "no live permission session", http.StatusServiceUnavailable)
		return
	}
	if s.Token == "" || !s.tokenOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	var body struct {
		State    string `json:"state"`
		Response string `json:"response"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	state := perm.State(body.State)
	if state != perm.StateApproved && state != perm.StateDenied {
		http.Error(w, "state must be approved or denied", http.StatusBadRequest)
		return
	}
	var (
		out perm.Outcome
		err error
	)
	if body.Response != "" {
		out, err = s.Perm.RespondAs(id, body.Response, "serve", "serve")
	} else {
		out, err = s.Perm.AnswerAs(id, state, "serve", "serve")
	}
	if err != nil {
		if errors.Is(err, perm.ErrChannelReadOnly) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if errors.Is(err, perm.ErrUnknown) || errors.Is(err, perm.ErrExpired) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"id": id, "state": out.Request.State, "by": out.Request.By, "won": out.Won,
	})
}

// tokenOK checks the local token (Authorization: Bearer or
// X-Owltide-Token).
func (s *Server) tokenOK(r *http.Request) bool {
	if h := r.Header.Get("X-Owltide-Token"); h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(s.Token)) == 1 {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(s.Token)) == 1
	}
	return false
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// Replay after Last-Event-ID, or a reset when behind the window.
	last := -1
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			last = n
		}
	}
	frames, reset := s.hub.replay(last)
	for _, f := range frames {
		if _, err := w.Write(f.data); err != nil {
			return
		}
	}
	if reset {
		if _, err := w.Write([]byte(s.hub.snapshotFrame("reset", map[string]any{}).data)); err != nil {
			return
		}
	}
	// Pending approvals must reach every new client until they reach a
	// terminal state (idempotent per request id).
	for _, req := range s.pendingApprovals() {
		if _, err := w.Write(s.hub.snapshotFrame("approval_pending", map[string]any{
			"id": req.ID, "kind": req.Kind, "source": req.Source.Agent, "payload": req.Payload,
		}).data); err != nil {
			return
		}
	}
	fl.Flush()

	// Heartbeat comments keep intermediaries from closing the stream.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case f, ok := <-ch:
			if !ok {
				return // stalled client dropped by the hub
			}
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

// ListenAndServe binds addr (127.0.0.1 by caller contract) and serves
// until the process exits. Addr is logged in plaintext; nothing
// secret is in scope of any handler.
func (s *Server) ListenAndServe(addr string) error {
	log.Printf("owltide serve: http://%s (read-only API)", addr)
	return http.ListenAndServe(addr, s.Handler())
}
