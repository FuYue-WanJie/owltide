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
