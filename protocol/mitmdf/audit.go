package mitmdf

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// auditDecision is one of the fixed verdicts recorded in the audit log.
type auditDecision string

const (
	auditAllow   auditDecision = "allow"    // a leaf was minted and the egress was attempted
	auditDeny    auditDecision = "deny"     // deny list matched; no mint
	auditNoMatch auditDecision = "no-match" // no fronts entry claimed the SNI; no mint
	auditError   auditDecision = "error"    // mint or egress failed; recorded for forensics
)

// auditRecord is the JSONL line schema. Stable on-disk; new fields must be
// added with `omitempty` so old log readers don't reject them.
type auditRecord struct {
	Timestamp  time.Time     `json:"ts"`
	SNI        string        `json:"sni"`
	FrontedSNI string        `json:"fronted_sni,omitempty"`
	Decision   auditDecision `json:"decision"`
	Reason     string        `json:"reason,omitempty"`
}

// auditLog is an append-only JSONL writer. nil receiver methods are safe
// no-ops so callers can use a single code path whether or not auditing is
// configured.
type auditLog struct {
	mu sync.Mutex
	f  *os.File
}

// openAuditLog opens (or creates) path for append at mode 0600. Returns
// nil, nil if path is empty (caller may disable auditing by leaving the
// config field unset).
func openAuditLog(path string) (*auditLog, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit log open %s: %w", path, err)
	}
	return &auditLog{f: f}, nil
}

// record writes one JSONL line under the file lock. Write errors are
// dropped on the floor — the audit log is best-effort by design: a full
// disk or a slow fsync must not block real traffic.
func (a *auditLog) record(r auditRecord) {
	if a == nil {
		return
	}
	r.Timestamp = time.Now().UTC()
	buf, err := json.Marshal(r)
	if err != nil {
		return
	}
	buf = append(buf, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.f.Write(buf)
}

// close releases the underlying file. Safe on nil.
func (a *auditLog) close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}
