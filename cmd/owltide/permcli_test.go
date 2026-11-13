package main

// M14 tests: the CLI endpoint routes ask_user and dangerous-action
// confirmations through the permission service while keeping the
// existing prompt text and error behaviour.

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FuYue-WanJie/owltide/core/agent"
	"github.com/FuYue-WanJie/owltide/internal/perm"
)

type eventLog struct {
	mu     sync.Mutex
	events []map[string]any
}

func (l *eventLog) add(f map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, f)
}

func (l *eventLog) all() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any{}, l.events...)
}

func newSvc(t *testing.T, timeout time.Duration) (*perm.Service, *eventLog) {
	t.Helper()
	log := &eventLog{}
	svc := perm.New(perm.Options{Timeout: timeout, Events: func(_ string, f map[string]any) {
		log.add(f)
	}})
	return svc, log
}

func TestAskViaPermApproved(t *testing.T) {
	svc, events := newSvc(t, time.Second)
	got, err := askViaPerm(svc, "s1",
		func(string, []string) (string, error) { return "42", nil }, "q?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "42" {
		t.Fatalf("answer = %q, want 42", got)
	}
	evs := events.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e["kind"] != "ask_user" || e["state"] != "approved" || e["by"] != "cli" || e["source"] != "main" {
		t.Fatalf("event = %v", e)
	}
	if _, has := e["payload"]; has {
		t.Fatalf("event must not expose payload: %v", e)
	}
}

func TestAskViaPermEOF(t *testing.T) {
	svc, _ := newSvc(t, time.Second)
	_, err := askViaPerm(svc, "s1",
		func(string, []string) (string, error) { return "", errors.New("no answer provided") }, "q?", nil)
	if err == nil || !strings.Contains(err.Error(), "no answer provided") {
		t.Fatalf("err = %v, want the legacy no-answer error", err)
	}
}

func TestAskViaPermTimeout(t *testing.T) {
	svc, events := newSvc(t, 40*time.Millisecond)
	got, err := askViaPerm(svc, "s1", func(string, []string) (string, error) {
		time.Sleep(120 * time.Millisecond) // user too slow
		return "late answer", nil
	}, "q?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != agent.AskTimeoutMessage {
		t.Fatalf("timeout answer = %q, want the documented message", got)
	}
	evs := events.all()
	if len(evs) != 1 || evs[0]["state"] != "timeout" {
		t.Fatalf("events = %v, want one timeout event", evs)
	}
}

func TestConfirmViaPerm(t *testing.T) {
	svc, _ := newSvc(t, time.Second)
	if !confirmViaPerm(svc, perm.Source{Agent: "main", Session: "s1"},
		func(string) bool { return true }, "allow?") {
		t.Fatal("yes must approve")
	}
	if confirmViaPerm(svc, perm.Source{Agent: "main", Session: "s1"},
		func(string) bool { return false }, "allow?") {
		t.Fatal("no must deny")
	}
}

func TestConfirmViaPermTimeoutDenies(t *testing.T) {
	svc, events := newSvc(t, 40*time.Millisecond)
	ok := confirmViaPerm(svc, perm.Source{Agent: "main", Session: "s1"},
		func(string) bool {
			time.Sleep(120 * time.Millisecond)
			return true // too late: timeout already denied
		}, "allow?")
	if ok {
		t.Fatal("timeout must deny")
	}
	evs := events.all()
	if len(evs) != 1 || evs[0]["state"] != "timeout" {
		t.Fatalf("events = %v, want one timeout event", evs)
	}
}
