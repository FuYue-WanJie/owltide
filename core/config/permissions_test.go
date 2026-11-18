package config

import (
	"testing"
	"time"
)

// TestPermissionTimeout: config item permissions.timeout_sec with the
// 300s default (P2b-M13).
func TestPermissionTimeout(t *testing.T) {
	if got := Default().PermissionTimeout(); got != 300*time.Second {
		t.Fatalf("default timeout = %v, want 300s", got)
	}
	if got := (Config{Permissions: Permissions{TimeoutSec: 45}}).PermissionTimeout(); got != 45*time.Second {
		t.Fatalf("configured timeout = %v, want 45s", got)
	}
	if got := (Config{Permissions: Permissions{TimeoutSec: -5}}).PermissionTimeout(); got != 300*time.Second {
		t.Fatalf("invalid timeout = %v, want 300s fallback", got)
	}
}

// TestCompactRatio: budget.compact_at default 0.7, 0 disables, and it
// never consults max_tokens (P2b-M6).
func TestCompactRatio(t *testing.T) {
	b := Default().Budget
	if got := b.CompactRatio(); got != 0.7 {
		t.Fatalf("default ratio = %v, want 0.7", got)
	}
	zero := 0.0
	if got := (Budget{CompactAt: &zero}).CompactRatio(); got != 0 {
		t.Fatalf("explicit 0 = %v, want disabled", got)
	}
	half := 0.5
	if got := (Budget{CompactAt: &half, MaxTokens: 400000}).CompactRatio(); got != 0.5 {
		t.Fatalf("explicit ratio = %v (max_tokens must not participate)", got)
	}
}
