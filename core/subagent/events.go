package subagent

// Audit events for sub-agent lifecycle, one JSON object per line in
// ~/.owltide/logs/subagents.jsonl. Only digests go in — never task or
// summary content.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var eventMu sync.Mutex

// eventFieldAllowlist: the only field keys written through verbatim.
// Everything else — error text, commands, paths, anything a future
// call site adds — is stored as a sha256 digest (key_digest), so task
// content or sensitive strings can never leak into the audit log
// regardless of length.
var eventFieldAllowlist = map[string]bool{
	"id":             true, // sub-<8 hex>
	"role":           true, // role name
	"mode":           true, // single|parallel|chain[-chain:<id>]
	"state":          true, // queued|running|done|stopped|failed
	"op":             true, // budget op: reserved|released
	"by":             true, // stop origin: agent_stop
	"reason":         true, // static refusal text (+ token counts)
	"summary_digest": true, // already a digest
	"chain":          true, // chain id, sub-<8 hex>
	"tokens":         true, // int
	"count":          true, // int
	"resumed":        true, // bool
}

// AppendEvent writes one event line with ts; field keys outside the
// allowlist are reduced to sha256 digests (key_digest).
func AppendEvent(logDir, event string, fields map[string]any) {
	rec := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339),
		"event": event,
	}
	for k, v := range fields {
		if eventFieldAllowlist[k] {
			rec[k] = v
			continue
		}
		sum := sha256.Sum256([]byte(fmt.Sprint(v)))
		rec[k+"_digest"] = hex.EncodeToString(sum[:8])
	}
	eventMu.Lock()
	defer eventMu.Unlock()
	os.MkdirAll(logDir, 0o755)
	f, err := os.OpenFile(filepath.Join(logDir, "subagents.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(rec)
	f.Write(append(b, '\n'))
}

// DeliveredIDs reads back subagents.jsonl and returns the set of
// sub-agent ids whose result was already delivered (delivered events).
// Used at startup to avoid re-delivering a result after a restart.
func DeliveredIDs(logDir string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(filepath.Join(logDir, "subagents.jsonl"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec struct {
			Event string `json:"event"`
			ID    string `json:"id"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) == nil &&
			rec.Event == "delivered" && rec.ID != "" {
			out[rec.ID] = true
		}
	}
	return out
}
