package serve

// Read-only HTTP API over the local runtime state: session list, one
// session's messages, sub-agent status, background job status, and an
// SSE stream pushing new messages and state changes. No write
// endpoints by design (the UI contract is not settled yet), and
// nothing secret ever leaves the process: API keys, endpoints and env
// contents are never loaded into the handler's reach.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	mu   sync.Mutex
	subs func() []map[string]any
	jobs func() []map[string]any
}

// SetWiring installs the snapshot functions (called once at startup).
func (st *State) SetWiring(subs, jobs func() []map[string]any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.subs, st.jobs = subs, jobs
}

// ---- HTTP handlers ----

// Server is the read-only API server.
type Server struct {
	Sessions Sessions
	State    *State
	hub      *hub
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

type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newHub() *hub { return &hub{clients: map[chan []byte]struct{}{}} }

func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *hub) broadcast(event string, data any) {
	payload, err := json.Marshal(map[string]any{"event": event, "data": data})
	if err != nil {
		return
	}
	frame := append([]byte("data: "), append(payload, '\n', '\n')...)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- frame:
		default: // slow client: drop the frame, keep the stream alive
		}
	}
}

// Publish lets the CLI push runtime events (new messages, state
// changes) into the SSE stream.
func (s *Server) Publish(event string, data any) { s.hub.broadcast(event, data) }

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
	// Heartbeat comments keep intermediaries from closing the stream.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-ch:
			_, _ = w.Write(frame)
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
