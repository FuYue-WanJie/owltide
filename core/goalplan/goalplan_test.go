package goalplan

// P2b-M1 tests: legal and illegal transitions (table driven), open
// todos, blocked reason requirement, unblock through the permission
// service (approved / denied / timeout / channel), frontmatter
// protection, atomic writes, and concurrent writes (-race).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FuYue-WanJie/owltide/core/store"
	"github.com/FuYue-WanJie/owltide/internal/perm"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	return New(t.TempDir())
}

func mustCreateGoal(t *testing.T, m *Manager, slug, body string) Doc {
	t.Helper()
	d, err := m.Create(KindGoal, slug, "s-test", body, "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustCreatePlan(t *testing.T, m *Manager, slug, body string) Doc {
	t.Helper()
	g := mustCreateGoal(t, m, slug+"-goal", "goal body")
	d, err := m.Create(KindPlan, slug, "s-test", body, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCreateAndParse(t *testing.T) {
	m := newMgr(t)
	d, err := m.Create(KindGoal, "add-tests", "s-1", "goal statement\n- [ ] criterion", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.FileName, time.Now().Format("20060102")+"-") ||
		!strings.HasSuffix(d.FileName, "-add-tests.md") {
		t.Fatalf("file name = %s", d.FileName)
	}
	if !strings.HasPrefix(d.ID, "g-") || len(d.ID) != len("g-")+8 {
		t.Fatalf("id = %s", d.ID)
	}

	loaded, err := m.Load(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kind != KindGoal || loaded.Status != StatusActive || loaded.Session != "s-1" {
		t.Fatalf("loaded = %+v", loaded)
	}
	if !strings.Contains(loaded.Body, "goal statement") {
		t.Fatalf("body lost: %q", loaded.Body)
	}

	// File name rules.
	if _, err := m.Create(KindGoal, "Bad_Slug", "s", "", ""); err == nil {
		t.Fatal("bad slug must be refused")
	}
	if err := ValidateFilename("x.md"); err == nil {
		t.Fatal("date-less file name must be refused")
	}
}

func TestTransitionsTable(t *testing.T) {
	cases := []struct {
		kind, from, to string
		body           string
		ok             bool
		wantErr        error
	}{
		{KindGoal, StatusActive, StatusDone, "", true, nil},
		{KindGoal, StatusActive, StatusBlocked, "blocked: waiting for user", true, nil},
		{KindGoal, StatusActive, StatusBlocked, "no reason", false, ErrBlockedNeedsReason},
		{KindGoal, StatusActive, StatusArchived, "", false, ErrInvalidTransition},
		{KindGoal, StatusDone, StatusActive, "", false, ErrInvalidTransition},
		{KindGoal, StatusDone, StatusArchived, "", true, nil},
		{KindGoal, StatusBlocked, StatusArchived, "blocked: x", true, nil},
		{KindGoal, StatusBlocked, StatusDone, "blocked: x", false, ErrInvalidTransition},
		{KindGoal, StatusArchived, StatusActive, "", false, ErrInvalidTransition},
		{KindPlan, StatusActive, StatusDone, "- [x] step", true, nil},
		{KindPlan, StatusActive, StatusDone, "- [ ] step", false, ErrOpenTodos},
		{KindPlan, StatusActive, StatusArchived, "- [ ] step", true, nil},
		{KindPlan, StatusActive, StatusBlocked, "blocked: x", false, ErrInvalidTransition},
		{KindPlan, StatusDone, StatusArchived, "", false, ErrInvalidTransition},
		{KindPlan, StatusArchived, StatusActive, "", false, ErrInvalidTransition},
	}
	for i, c := range cases {
		m := newMgr(t)
		var id string
		if c.kind == KindGoal {
			id = mustCreateGoal(t, m, fmt.Sprintf("doc-%02d", i), c.body).ID
		} else {
			id = mustCreatePlan(t, m, fmt.Sprintf("doc-%02d", i), c.body).ID
		}
		// Move to the from state first (only needed for done/blocked/
		// archived).
		if c.from == StatusDone {
			if _, _, _, err := m.SetStatus(id, StatusDone, nil); err != nil && c.kind == KindGoal {
				// goal -> done needs no todo check.
				t.Fatalf("case %d setup: %v", i, err)
			}
		}
		if c.from == StatusBlocked {
			if _, _, _, err := m.SetStatus(id, StatusBlocked, nil); err != nil {
				t.Fatalf("case %d setup blocked: %v", i, err)
			}
		}
		if c.from == StatusArchived {
			cur, lerr := m.Load(id)
			if lerr != nil {
				t.Fatal(lerr)
			}
			via := StatusDone
			if c.kind == KindPlan {
				via = StatusActive // plan: active -> archived directly
			}
			if cur.Status != via {
				if _, _, _, serr := m.SetStatus(id, via, func(planID string) (bool, error) {
					doc, lerr2 := m.Load(planID)
					if lerr2 != nil {
						return false, lerr2
					}
					return TodosAllDone(doc.Body), nil
				}); serr != nil {
					t.Fatalf("case %d setup archived (%s): %v", i, via, serr)
				}
			}
			if _, _, _, serr := m.SetStatus(id, StatusArchived, nil); serr != nil {
				t.Fatalf("case %d setup archived: %v", i, serr)
			}
		}
		checker := func(planID string) (bool, error) {
			doc, err := m.Load(planID)
			if err != nil {
				return false, err
			}
			return TodosAllDone(doc.Body), nil
		}
		d, _, needConfirm, err := m.SetStatus(id, c.to, checker)
		if c.ok {
			if err != nil {
				t.Fatalf("case %d (%s %s->%s): %v", i, c.kind, c.from, c.to, err)
			}
			if needConfirm {
				t.Fatalf("case %d: unexpected confirmation request", i)
			}
			if d.Status != c.to {
				t.Fatalf("case %d: status = %s", i, d.Status)
			}
			// Persisted.
			loaded, _ := m.Load(id)
			if loaded.Status != c.to {
				t.Fatalf("case %d: persisted status = %s", i, loaded.Status)
			}
		} else {
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("case %d (%s %s->%s): err = %v, want %v", i, c.kind, c.from, c.to, err, c.wantErr)
			}
			// File untouched.
			loaded, _ := m.Load(id)
			if loaded.Status != c.from {
				t.Fatalf("case %d: failed transition must not touch the file, got %s", i, loaded.Status)
			}
		}
	}
}

func TestSameStatusRefused(t *testing.T) {
	m := newMgr(t)
	id := mustCreateGoal(t, m, "same", "").ID
	if _, _, _, err := m.SetStatus(id, StatusActive, nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("same-status = %v", err)
	}
}

func TestUnblockConfirmation(t *testing.T) {
	cases := []struct {
		name     string
		approved bool
		askErr   error
		wantErr  string
	}{
		{"approved", true, nil, ""},
		{"declined", false, nil, "user declined"},
		{"timeout", false, errors.New("timeout"), "not confirmed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMgr(t)
			g := mustCreateGoal(t, m, "blk", "blocked: waiting")
			if _, _, _, err := m.SetStatus(g.ID, StatusBlocked, nil); err != nil {
				t.Fatal(err)
			}
			h := &Hooks{M: m, ConfirmUnblock: func(string, string) (bool, error) {
				return c.approved, c.askErr
			}}
			ut := updateTool{h, KindGoal}
			out, err := ut.Call(context.Background(), json.RawMessage(`{"id":"`+g.ID+`","status":"active"}`))
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out, "unblocked") {
					t.Fatalf("out = %q", out)
				}
				loaded, _ := m.Load(g.ID)
				if loaded.Status != StatusActive {
					t.Fatalf("status = %s", loaded.Status)
				}
				// The reason line is gone.
				if strings.Contains(loaded.Body, blockedPrefix) {
					t.Fatalf("reason line kept: %q", loaded.Body)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %s", err, c.wantErr)
			}
			loaded, _ := m.Load(g.ID)
			if loaded.Status != StatusBlocked {
				t.Fatalf("denied unblock must keep blocked, got %s", loaded.Status)
			}
			if !strings.HasPrefix(loaded.Body, blockedPrefix) {
				t.Fatalf("reason line must survive a refused unblock: %q", loaded.Body)
			}
		})
	}
}

func TestUnblockThroughPermService(t *testing.T) {
	cases := []struct {
		name    string
		answer  func(svc *perm.Service, id string, errs chan<- error)
		wantErr string
	}{
		{"approved", func(svc *perm.Service, id string, errs chan<- error) {
			go func() {
				time.Sleep(50 * time.Millisecond)
				if _, rerr := svc.Respond(id, "yes", "cli"); rerr != nil {
					errs <- rerr
				}
			}()
		}, ""},
		{"denied", func(svc *perm.Service, id string, errs chan<- error) {
			go func() {
				time.Sleep(50 * time.Millisecond)
				if _, aerr := svc.Answer(id, perm.StateDenied, "cli"); aerr != nil {
					errs <- aerr
				}
			}()
		}, "stays blocked"},
		{"timeout", func(*perm.Service, string, chan<- error) {}, "stays blocked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMgr(t)
			g := mustCreateGoal(t, m, "blk", "blocked: need user")
			if _, _, _, err := m.SetStatus(g.ID, StatusBlocked, nil); err != nil {
				t.Fatal(err)
			}
			svc := perm.New(perm.Options{Timeout: 2 * time.Second})
			errs := make(chan error, 4)
			h := &Hooks{M: m, SessionID: "s1", Perm: svc}
			h.ConfirmUnblock = func(goalID, reason string) (bool, error) {
				req, err := svc.Ask(perm.Kind("goal_unblock"),
					perm.Source{Agent: "main", Session: "s1"},
					map[string]string{"goal": goalID, "reason": reason})
				if err != nil {
					return false, err
				}
				// The answering endpoint races the timeout (M15/M16
				// semantics).
				c.answer(svc, req.ID, errs)
				out, err := svc.Wait(context.Background(), req.ID)
				if err != nil {
					return false, err
				}
				if out.Request.State != perm.StateApproved {
					return false, fmt.Errorf("request %s (%s)", req.ID, out.Request.State)
				}
				return true, nil
			}
			ut := updateTool{h, KindGoal}
			_, err := ut.Call(context.Background(), json.RawMessage(`{"id":"`+g.ID+`","status":"active"}`))
			select {
			case e := <-errs:
				t.Fatalf("async answer failed: %v", e)
			default:
			}
			if c.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %s", err, c.wantErr)
			}
			loaded, _ := m.Load(g.ID)
			if loaded.Status != StatusBlocked {
				t.Fatalf("must stay blocked, got %s", loaded.Status)
			}
		})
	}
}

// TestChannelSourceRefused: a social-channel session cannot even ask
// for the unblock (read-only policy, P2b-M15).
func TestChannelSourceRefused(t *testing.T) {
	svc := perm.New(perm.Options{Timeout: time.Second})
	_, err := svc.Ask(perm.Kind("goal_unblock"),
		perm.Source{Agent: "main", Channel: "qq", Session: "s1"}, nil)
	if !errors.Is(err, perm.ErrChannelReadOnly) {
		t.Fatalf("err = %v, want ErrChannelReadOnly", err)
	}
}

// TestConcurrentWrites: two goroutines writing the same plan race; the
// atomic write (temp+rename) never leaves a torn file, and both parsed
// documents are valid.
func TestConcurrentWrites(t *testing.T) {
	m := newMgr(t)
	p := mustCreatePlan(t, m, "race", "- [x] step")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			loaded, err := m.Load(p.ID)
			if err != nil {
				errs <- err
				return
			}
			loaded.Body = "- [x] step\n\nvariant " + strconv2(i)
			if err := m.Save(loaded); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.File)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parse(p.File, p.FileName, string(data)); err != nil {
		t.Fatalf("torn file after concurrent writes: %v", err)
	}
}

func strconv2(i int) string { return "v" + string(rune('0'+i)) }

// TestTempFilesNotLeft: no .tmp files survive successful writes.
func TestTempFilesNotLeft(t *testing.T) {
	m := newMgr(t)
	g := mustCreateGoal(t, m, "tmp", "body")
	for i := 0; i < 3; i++ {
		g.Body = "body v" + strconv2(i)
		if err := m.Save(g); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(g.File))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// TestBodyRewriteKeepsFrontmatter: the body path never invalidates the
// frontmatter, and a blocked goal keeps its reason line.
func TestBodyRewriteKeepsFrontmatter(t *testing.T) {
	m := newMgr(t)
	g := mustCreateGoal(t, m, "body", "original")
	g2, err := m.Load(g.ID)
	if err != nil {
		t.Fatal(err)
	}
	g2.Body = "updated body"
	if err := m.Save(g2); err != nil {
		t.Fatal(err)
	}
	loaded, _ := m.Load(g.ID)
	if loaded.Status != StatusActive || loaded.ID != g.ID {
		t.Fatalf("frontmatter damaged: %+v", loaded)
	}
	if loaded.Body != "updated body" {
		t.Fatalf("body = %q", loaded.Body)
	}

	// A blocked goal's body rewrite must keep (or restore) the reason.
	blk, lerr := m.Load(g.ID)
	blk.Body = blockedPrefix + "waiting on user"
	if lerr != nil {
		t.Fatal(lerr)
	}
	if serr := m.Save(blk); serr != nil {
		t.Fatal(serr)
	}
	if _, _, _, serr2 := m.SetStatus(g.ID, StatusBlocked, nil); serr2 != nil {
		t.Fatal(serr2)
	}
	blk2, lerr2 := m.Load(g.ID)
	if lerr2 != nil {
		t.Fatal(lerr2)
	}
	blk2.Body = blockedPrefix + "still waiting\nmore detail"
	if serr3 := m.Save(blk2); serr3 != nil {
		t.Fatal(serr3)
	}
	ut := updateTool{&Hooks{M: m}, KindGoal}
	if _, err := ut.Call(context.Background(), json.RawMessage(`{"id":"`+g.ID+`","body":"reason lost"}`)); err == nil {
		t.Fatal("blocked body rewrite without a reason must be refused")
	}
}

func TestRefsFromBody(t *testing.T) {
	goals, plans := RefsFromBody("see goal:g-0123abcd and plan:p-ffffffff, again goal:g-0123abcd")
	if len(goals) != 1 || goals[0] != "g-0123abcd" {
		t.Fatalf("goals = %v", goals)
	}
	if len(plans) != 1 || plans[0] != "p-ffffffff" {
		t.Fatalf("plans = %v", plans)
	}
}

func TestFrontmatterSpan(t *testing.T) {
	text := "---\nid: g-x\n---\nbody"
	start, end, ok := FrontmatterSpan(text)
	if !ok || start != 0 || text[start:end] != "---\nid: g-x\n---" {
		t.Fatalf("span = %d,%d ok=%v", start, end, ok)
	}
	if _, _, ok := FrontmatterSpan("no frontmatter"); ok {
		t.Fatal("plain text must have no span")
	}
}

// TestContextCanceledUnblock: a canceled waiter does not resolve the
// request and the goal stays blocked.
func TestContextCanceledUnblock(t *testing.T) {
	m := newMgr(t)
	g := mustCreateGoal(t, m, "blk", "blocked: x")
	if _, _, _, err := m.SetStatus(g.ID, StatusBlocked, nil); err != nil {
		t.Fatal(err)
	}
	svc := perm.New(perm.Options{Timeout: 5 * time.Second})
	h := &Hooks{M: m, SessionID: "s1"}
	h.ConfirmUnblock = func(goalID, reason string) (bool, error) {
		req, err := svc.Ask(perm.Kind("goal_unblock"), perm.Source{Agent: "main", Session: "s1"}, nil)
		if err != nil {
			return false, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = svc.Wait(ctx, req.ID)
		return false, err
	}
	ut := updateTool{h, KindGoal}
	if _, err := ut.Call(context.Background(), json.RawMessage(`{"id":"`+g.ID+`","status":"active"}`)); err == nil {
		t.Fatal("canceled confirmation must fail")
	}
	loaded, _ := m.Load(g.ID)
	if loaded.Status != StatusBlocked {
		t.Fatalf("status = %s, want blocked", loaded.Status)
	}
}

// ---- M2 tests ----

// TestInjectContentAndOrder: active goals and plans appear with ids,
// statements and steps; done/archived are excluded.
func TestInjectContentAndOrder(t *testing.T) {
	m := newMgr(t)
	g := mustCreateGoal(t, m, "ship", "ship the feature\n- [ ] tests pass\n- [x] design")
	mustCreatePlan(t, m, "ship-steps", "- [x] design\n- [ ] implement")
	// A done goal must not appear.
	done := mustCreateGoal(t, m, "old", "old goal")
	if _, _, _, err := m.SetStatus(done.ID, StatusDone, nil); err != nil {
		t.Fatal(err)
	}
	text := (Injector{M: m}).Inject()
	if !strings.Contains(text, "[AUTO — not a new user message]") {
		t.Fatalf("marker missing:\n%s", text)
	}
	if !strings.Contains(text, "GOAL "+g.ID) || !strings.Contains(text, "ship the feature") {
		t.Fatalf("goal content missing:\n%s", text)
	}
	if strings.Contains(text, "old goal") {
		t.Fatalf("done goal leaked:\n%s", text)
	}
	if !strings.Contains(text, "- [ ] implement") {
		t.Fatalf("plan steps missing:\n%s", text)
	}
}

// TestInjectOversizedListsIdsOnly: beyond the 2000-char budget the
// injection degrades to ids and file names.
func TestInjectOversizedListsIdsOnly(t *testing.T) {
	m := newMgr(t)
	for i := 0; i < 12; i++ {
		g := mustCreateGoal(t, m, fmt.Sprintf("big-%02d", i), strings.Repeat("x ", 300)+"\nacceptance")
		if _, _, _, err := m.SetStatus(g.ID, StatusDone, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		mustCreateGoal(t, m, fmt.Sprintf("wide-%02d", i), strings.Repeat("y ", 300))
	}
	text := (Injector{M: m}).Inject()
	if len(text) > InjectBudget {
		t.Fatalf("injection over budget: %d", len(text))
	}
	// The full 600-char bodies are gone: the injection degraded to
	// first-line summaries (and would degrade further to ids-only if
	// even that overflowed).
	if strings.Contains(text, strings.Repeat("y ", 300)) {
		t.Fatalf("full body leaked past the budget:\n%s", text)
	}
	if !strings.Contains(text, "GOAL ") {
		t.Fatalf("goal ids missing:\n%s", text)
	}
}

// TestInjectBeyondBriefGoesIdsOnly: with enough active goals even the
// brief form overflows; the ids-only form must then apply and still
// respect the budget.
func TestInjectBeyondBriefGoesIdsOnly(t *testing.T) {
	m := newMgr(t)
	for i := 0; i < 40; i++ {
		mustCreateGoal(t, m, fmt.Sprintf("mass-%02d", i), strings.Repeat("z ", 120))
	}
	text := (Injector{M: m}).Inject()
	if len(text) > InjectBudget {
		t.Fatalf("over budget: %d", len(text))
	}
	if !strings.Contains(text, "仅 id") {
		t.Fatalf("ids-only form missing:\n%s", text)
	}
}

// TestInjectEmptyWhenNothingActive.
func TestInjectEmptyWhenNothingActive(t *testing.T) {
	m := newMgr(t)
	if got := (Injector{M: m}).Inject(); got != "" {
		t.Fatalf("empty store inject = %q", got)
	}
}

// TestEventsOnTransitions: terminal transitions emit goal_update /
// plan_update with id/from/to and no body text.
func TestEventsOnTransitions(t *testing.T) {
	var evs []map[string]any
	h := &Hooks{
		M: newMgr(t), SessionID: "s1",
		Events: func(name string, f map[string]any) { evs = append(evs, f) },
	}
	g, err := h.M.Create(KindGoal, "ev", "s1", "body", "")
	if err != nil {
		t.Fatal(err)
	}
	ut := updateTool{h, KindGoal}
	if _, err := ut.Call(context.Background(), json.RawMessage(`{"id":"`+g.ID+`","status":"done"}`)); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %v", evs)
	}
	e := evs[0]
	if e["id"] != g.ID || e["from"] != StatusActive || e["to"] != StatusDone {
		t.Fatalf("event = %v", e)
	}
	if _, has := e["body"]; has {
		t.Fatalf("event must not carry the body: %v", e)
	}
}

// TestStoreAutoCommitLeavesGitClean: a create + transition through the
// tool hooks auto-commits the store; the real git status stays clean.
func TestStoreAutoCommitLeavesGitClean(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureLayout(dir); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureInit(); err != nil {
		t.Fatal(err)
	}
	m := New(dir)
	commits := 0
	h := &Hooks{
		M: m, SessionID: "s1",
		OnStoreChange: func(reason string) {
			if _, _, err := st.AutoCommit(store.CommitMeta{
				Who: "agent", Tool: "goalplan", Game: "s1", Reason: reason,
			}); err != nil {
				t.Fatal(err)
			}
			commits++
		},
	}
	ut := updateTool{h, KindGoal}
	d, err := h.M.Create(KindGoal, "clean", "s1", "body", "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the tool path (create via tool so OnStoreChange fires).
	gc := goalCreate{h}
	if _, err := gc.Call(context.Background(), json.RawMessage(`{"slug":"clean2","body":"b"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := ut.Call(context.Background(), json.RawMessage(`{"id":"`+d.ID+`","status":"done"}`)); err != nil {
		t.Fatal(err)
	}
	if commits == 0 {
		t.Fatal("no auto-commit happened")
	}
	// The store's git tree must be clean: a follow-up AutoCommit with
	// no changes reports changed=false (work tree == HEAD).
	_, changed, err := st.AutoCommit(store.CommitMeta{
		Who: "agent", Tool: "goalplan", Game: "s1", Reason: "no-op check",
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("store git status not clean after auto-commit (work tree differs from HEAD)")
	}
}
