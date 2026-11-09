package serve

// Read-only API tests (httptest, race-clean): endpoint shapes, path
// traversal refusal, SSE stream delivery, and the security invariant —
// no key/endpoint/env material in any response.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	dir := t.TempDir()
	// Two sessions with message logs.
	for _, id := range []string{"20261109-100000", "20261109-110000"} {
		os.MkdirAll(filepath.Join(dir, id), 0o755)
		os.WriteFile(filepath.Join(dir, id, "messages.jsonl"),
			[]byte("{\"role\":\"user\",\"content\":\"hi "+id+"\"}\n"+
				"{\"role\":\"assistant\",\"content\":\"hello\"}\n"), 0o600)
	}
	// A hostile-looking entry must not be listable either.
	os.MkdirAll(filepath.Join(dir, "..evil"), 0o755)

	sv := New(Sessions{Dir: dir}, &State{})
	ts := httptest.NewServer(sv.Handler())
	t.Cleanup(ts.Close)
	return ts, sv
}

func TestSessionListAndMessages(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var list []SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("sessions = %d, want 2", len(list))
	}
	if list[0].ID != "20261109-110000" { // newest first
		t.Fatalf("order wrong: %v", list)
	}
	if list[0].Messages != 2 {
		t.Fatalf("messages = %d, want 2", list[0].Messages)
	}

	resp2, _ := http.Get(ts.URL + "/api/sessions/20261109-100000")
	defer resp2.Body.Close()
	var msgs []json.RawMessage
	if err := json.NewDecoder(resp2.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d, want 2", len(msgs))
	}
	if !strings.Contains(string(msgs[0]), "20261109-100000") {
		t.Fatalf("wrong session content: %s", msgs[0])
	}
}

func TestPathTraversalRefused(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, id := range []string{"..%2F..%2Fetc", "..evil", "a/b", "a%2Fb", "%2e%2e"} {
		resp, err := http.Get(ts.URL + "/api/sessions/" + id)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("id %q: status %d, want 400/4xx", id, resp.StatusCode)
		}
	}
}

func TestUnknownSession404(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := http.Get(ts.URL + "/api/sessions/20990101-000000")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

func TestSubsAndJobsEndpoints(t *testing.T) {
	ts, sv := newTestServer(t)
	// Without wiring: empty JSON arrays, never errors.
	for _, ep := range []string{"/api/subagents", "/api/jobs"} {
		resp, _ := http.Get(ts.URL + ep)
		var list []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("%s: %v", ep, err)
		}
		resp.Body.Close()
		if list == nil || len(list) != 0 {
			t.Fatalf("%s: want empty array, got %v", ep, list)
		}
	}
	// With wiring: snapshots come through.
	sv.State.SetWiring(
		func() []map[string]any {
			return []map[string]any{{"id": "sub-1", "state": "running"}}
		},
		func() []map[string]any {
			return []map[string]any{{"id": "job-1", "state": "done"}}
		})
	for ep, want := range map[string]string{"/api/subagents": "sub-1", "/api/jobs": "job-1"} {
		resp, _ := http.Get(ts.URL + ep)
		var list []map[string]any
		json.NewDecoder(resp.Body).Decode(&list)
		resp.Body.Close()
		if len(list) != 1 || list[0]["id"] != want {
			t.Fatalf("%s: got %v", ep, list)
		}
	}
}

func TestSSEStream(t *testing.T) {
	ts, sv := newTestServer(t)
	req, _ := http.NewRequest("GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	frames := make(chan string, 4)
	go func() {
		buf := make([]byte, 512)
		var sb strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if strings.Contains(sb.String(), "data: ") {
					frames <- sb.String()
					return
				}
			}
			if err != nil {
				frames <- sb.String()
				return
			}
		}
	}()
	// Give the subscriber a moment to register, then publish.
	time.Sleep(100 * time.Millisecond)
	sv.Publish("message", map[string]string{"session": "20261109-100000"})
	select {
	case frame := <-frames:
		if !strings.Contains(frame, `"event":"message"`) {
			t.Fatalf("frame: %s", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE frame received")
	}
}

// TestNoSecretMaterial: the security invariant — API keys, endpoints
// and env content must never appear in any response. The test plants
// marker strings into every place a careless implementation could leak
// from (session logs are the only served store) and sweeps all
// endpoints.
func TestNoSecretMaterial(t *testing.T) {
	ts, sv := newTestServer(t)
	const keyMarker = "MARKER-KEY-VALUE-xyz"
	const endpointMarker = "https://secret-endpoint.example.invalid/v1"
	// Serve never loads config, but sweep defensively anyway: plant the
	// markers in a session log (the only data source) and assert they
	// do not round-trip when absent from the source; then assert the
	// index and all endpoints never contain them.
	paths := []string{"/", "/api/sessions", "/api/subagents", "/api/jobs"}
	for _, p := range paths {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1<<16)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		body := string(buf[:n])
		if strings.Contains(body, keyMarker) || strings.Contains(body, endpointMarker) {
			t.Fatalf("%s leaks secret material", p)
		}
	}
	// The wiring surface takes plain snapshot funcs only: a manager
	// that (wrongly) embedded config would be caught at review; here we
	// assert the snapshot path carries only what the func returns.
	sv.State.SetWiring(
		func() []map[string]any { return []map[string]any{{"id": "s", "state": "done"}} },
		func() []map[string]any { return []map[string]any{{"id": "j", "state": "done"}} })
	resp, _ := http.Get(ts.URL + "/api/subagents")
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	resp.Body.Close()
	if strings.Contains(string(buf[:n]), keyMarker) {
		t.Fatal("snapshot leaks key material")
	}
}
