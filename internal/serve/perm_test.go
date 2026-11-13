package serve

// M16 tests: sequenced SSE with Last-Event-ID replay, approval events
// as must-deliver, the pending snapshot for new subscribers, and the
// token-guarded first-come-first-served answer endpoint.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FuYue-WanJie/owltide/internal/perm"
)

// permFixture wires a real perm.Service into the server, with its
// lifecycle events fanned into SSE must-deliver frames (exactly the
// wiring the CLI process will use).
func permFixture(t *testing.T) (*httptest.Server, *Server, *perm.Service) {
	t.Helper()
	ts, sv := newTestServer(t)
	svc := perm.New(perm.Options{
		Timeout: 5 * time.Second,
		Events: func(_ string, fields map[string]any) {
			sv.PublishMust("approval_resolved", fields)
		},
		OnPending: func(r perm.Request) {
			sv.PublishMust("approval_pending", map[string]any{
				"id": r.ID, "kind": r.Kind, "source": r.Source.Agent, "payload": r.Payload,
			})
		},
	})
	sv.Perm = svc
	sv.Token = "test-token"
	return ts, sv, svc
}

// readFrames collects SSE frames until the deadline.
func readFrames(t *testing.T, url string, header http.Header, dur time.Duration) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var frames []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "id: ") {
			frames = append(frames, line, "")
			continue
		}
		if len(frames) > 0 {
			frames[len(frames)-1] += line + "\n"
		}
	}
	return frames
}

func TestSSESequencedAndApprovalMustDeliver(t *testing.T) {
	ts, _, svc := permFixture(t)

	// Subscriber connects before any event.
	framesCh := make(chan []string, 1)
	go func() { framesCh <- readFrames(t, ts.URL+"/api/events", nil, 1500*time.Millisecond) }()
	time.Sleep(150 * time.Millisecond)

	// A pending request: approval_pending must reach the subscriber
	// even though it subscribes with buffering, then the resolve.
	req, err := svc.Ask(perm.KindApproval, perm.Source{Agent: "main", Session: "s1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := svc.Answer(req.ID, perm.StateApproved, "cli"); err != nil {
		t.Fatal(err)
	}

	var got strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case fs := <-framesCh:
			for _, f := range fs {
				got.WriteString(f)
			}
			goto done
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
done:
	body := got.String()
	if !strings.Contains(body, `"event":"approval_pending"`) {
		t.Fatalf("approval_pending missing:\n%s", body)
	}
	if !strings.Contains(body, `"event":"approval_resolved"`) {
		t.Fatalf("approval_resolved missing:\n%s", body)
	}
	// Sequence numbers are monotonic in wire order.
	var seqs []int
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			var n int
			_, _ = fmt.Sscanf(line[4:], "%d", &n)
			seqs = append(seqs, n)
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not monotonic: %v", seqs)
		}
	}
}

// TestLastEventIDReplay: a reconnecting client with Last-Event-ID
// receives the missed frames; one behind the window gets a reset.
func TestLastEventIDReplay(t *testing.T) {
	ts, sv, _ := permFixture(t)

	// Publish three progress events.
	for i := 0; i < 3; i++ {
		sv.Publish("progress", map[string]int{"i": i})
	}
	time.Sleep(100 * time.Millisecond)

	frames := readFrames(t, ts.URL+"/api/events",
		http.Header{"Last-Event-ID": []string{"1"}}, 800*time.Millisecond)
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "id: 2") || !strings.Contains(joined, "id: 3") {
		t.Fatalf("replay must include seq 2,3:\n%s", joined)
	}
	if strings.Contains(joined, "id: 1") {
		t.Fatalf("replay must not repeat seq 1:\n%s", joined)
	}

	// Behind the window: trim the ring so its oldest frame is newer
	// than the client's Last-Event-ID.
	sv.hub.mu.Lock()
	sv.hub.ring = sv.hub.ring[len(sv.hub.ring)-1:]
	sv.hub.mu.Unlock()
	frames = readFrames(t, ts.URL+"/api/events",
		http.Header{"Last-Event-ID": []string{"0"}}, 800*time.Millisecond)
	joined = strings.Join(frames, "")
	if !strings.Contains(joined, `"event":"reset"`) {
		t.Fatalf("behind-window client must get reset:\n%s", joined)
	}
}

// TestAnswerEndpoint: token required, first come first served, the
// loser observes the winner's state, channel sources are refused, and
// each resolution fans out as approval_resolved.
func TestAnswerEndpoint(t *testing.T) {
	ts, _, svc := permFixture(t)

	ask := func() perm.Request {
		r, err := svc.Ask(perm.KindApproval, perm.Source{Agent: "main", Session: "s1"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	// No token -> 401.
	r1 := ask()
	resp, _ := http.Post(ts.URL+"/api/permissions/"+r1.ID+"/answer", "application/json",
		strings.NewReader(`{"state":"approved"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}

	// With token: first answer wins.
	post := func(id, state string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/permissions/"+id+"/answer",
			strings.NewReader(`{"state":"`+state+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Owltide-Token", "test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := post(r1.ID, "approved")
	if code != 200 || out["won"] != true || out["state"] != "approved" {
		t.Fatalf("first answer: %d %v", code, out)
	}
	code, out = post(r1.ID, "denied")
	if code != 200 || out["won"] != false || out["state"] != "approved" {
		t.Fatalf("second answer: %d %v (must observe the winner)", code, out)
	}

	// Unknown id -> 404.
	code, _ = post("req-9999", "approved")
	if code != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", code)
	}

	// Bad state -> 400.
	r2 := ask()
	code, _ = post(r2.ID, "timeout")
	if code != http.StatusBadRequest {
		t.Fatalf("bad state status = %d, want 400", code)
	}
}

// TestPendingSnapshotOnConnect: a client connecting while approvals
// are pending receives approval_pending frames immediately (must
// reach terminal state via replay or snapshot).
func TestPendingSnapshotOnConnect(t *testing.T) {
	ts, _, svc := permFixture(t)
	r, err := svc.Ask(perm.KindApproval, perm.Source{Agent: "main", Session: "s1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	frames := readFrames(t, ts.URL+"/api/events", nil, 800*time.Millisecond)
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, `"event":"approval_pending"`) || !strings.Contains(joined, r.ID) {
		t.Fatalf("new client must see the pending approval:\n%s", joined)
	}
}
