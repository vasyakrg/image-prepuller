package auth

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"sync"
	"time"
)

// Audit actions recorded by the hub. ActionAuthorize is emitted by RequireRole
// for denied requests; the rest are recorded by the hub handlers.
const (
	ActionLogin        = "login"
	ActionLogout       = "logout"
	ActionAuthorize    = "authorize"
	ActionPull         = "pull"
	ActionWorkloadPull = "workload-pull"
	ActionCancelPull   = "cancel-pull"
	ActionReconcile    = "reconcile"
)

// Audit results.
const (
	ResultOK     = "ok"
	ResultDenied = "denied"
	ResultError  = "error"
)

// AuditEvent is one security-relevant action. It deliberately has no free-form
// details field: passwords, cookies and tokens can never be attached to it.
type AuditEvent struct {
	Time       time.Time `json:"time"`
	Actor      string    `json:"actor"`
	Role       string    `json:"role,omitempty"`
	Action     string    `json:"action"`
	Target     string    `json:"target,omitempty"`
	Result     string    `json:"result"`
	RemoteAddr string    `json:"remoteAddr,omitempty"`
}

// Auditor receives audit events. Implementations must be safe for concurrent
// use.
type Auditor interface {
	Log(ctx context.Context, ev AuditEvent)
}

// NopAuditor drops every event.
type NopAuditor struct{}

// Log implements Auditor.
func (NopAuditor) Log(context.Context, AuditEvent) {}

// JSONAuditor writes one JSON object per line to an io.Writer.
type JSONAuditor struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

// NewJSONAuditor returns an auditor writing newline-delimited JSON to w.
func NewJSONAuditor(w io.Writer) *JSONAuditor {
	return &JSONAuditor{w: w, now: time.Now}
}

// WithClock overrides the auditor clock (tests).
func (a *JSONAuditor) WithClock(now func() time.Time) *JSONAuditor {
	if a != nil && now != nil {
		a.now = now
	}
	return a
}

// Log implements Auditor.
func (a *JSONAuditor) Log(_ context.Context, ev AuditEvent) {
	if a == nil || a.w == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = a.now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = json.NewEncoder(a.w).Encode(ev)
}

// TeeAuditor fans every event out to several auditors (for example a JSON log
// plus an in-memory buffer exposed by the audit API).
type TeeAuditor struct {
	auditors []Auditor
}

// NewTeeAuditor returns an auditor forwarding to all non-nil auditors.
func NewTeeAuditor(auditors ...Auditor) *TeeAuditor {
	out := &TeeAuditor{}
	for _, a := range auditors {
		if a != nil {
			out.auditors = append(out.auditors, a)
		}
	}
	return out
}

// Log implements Auditor.
func (t *TeeAuditor) Log(ctx context.Context, ev AuditEvent) {
	if t == nil {
		return
	}
	for _, a := range t.auditors {
		a.Log(ctx, ev)
	}
}

// Recent returns buffered events from every auditor that keeps them, oldest
// first.
func (t *TeeAuditor) Recent() []AuditEvent {
	if t == nil {
		return nil
	}
	var out []AuditEvent
	for _, a := range t.auditors {
		if reader, ok := a.(interface{ Recent() []AuditEvent }); ok {
			out = append(out, reader.Recent()...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// MemoryAuditor keeps the most recent events in a bounded ring buffer. It backs
// the operator-only audit API.
type MemoryAuditor struct {
	mu     sync.Mutex
	limit  int
	events []AuditEvent
}

// NewMemoryAuditor returns an in-memory auditor keeping at most limit events
// (default 200, minimum 1).
func NewMemoryAuditor(limit int) *MemoryAuditor {
	if limit <= 0 {
		limit = 200
	}
	return &MemoryAuditor{limit: limit}
}

// Log implements Auditor.
func (a *MemoryAuditor) Log(_ context.Context, ev AuditEvent) {
	if a == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.events) == a.limit {
		copy(a.events, a.events[1:])
		a.events[len(a.events)-1] = ev
		return
	}
	a.events = append(a.events, ev)
}

// Recent returns a copy of the buffered events, oldest first.
func (a *MemoryAuditor) Recent() []AuditEvent {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEvent, len(a.events))
	copy(out, a.events)
	return out
}
