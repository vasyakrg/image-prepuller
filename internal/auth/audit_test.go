package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJSONAuditorWritesJSONLines(t *testing.T) {
	var buf bytes.Buffer
	auditor := NewJSONAuditor(&buf)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	auditor.WithClock(func() time.Time { return now })

	auditor.Log(nil, AuditEvent{Actor: "admin", Role: string(RoleOperator), Action: ActionPull, Target: "img", Result: ResultOK})
	auditor.Log(nil, AuditEvent{Actor: "view", Role: string(RoleViewer), Action: ActionPull, Target: "img", Result: ResultDenied, RemoteAddr: "10.0.0.1"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d: %q", len(lines), buf.String())
	}
	var ev AuditEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if ev.Actor != "admin" || ev.Action != ActionPull || ev.Result != ResultOK || !ev.Time.Equal(now) {
		t.Fatalf("unexpected event: %+v", ev)
	}
	var second AuditEvent
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2 is not JSON: %v", err)
	}
	if second.RemoteAddr != "10.0.0.1" || second.Result != ResultDenied {
		t.Fatalf("unexpected event: %+v", second)
	}
}

// TestAuditNeverContainsSecrets is the security test required by the spec: the
// password, the session cookie value and the raw session token must never reach
// the audit sink, even for failed logins and denied requests.
func TestAuditNeverContainsSecrets(t *testing.T) {
	const secretPassword = "hunter2-top-secret-password"
	var buf bytes.Buffer
	auditor := NewJSONAuditor(&buf)
	m := newTestManager(t, WithAuditor(auditor))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.RemoteAddr = "10.9.9.9:1234"
	if _, ok := m.Login(req, "admin", secretPassword); ok {
		t.Fatal("expected login failure")
	}
	if _, ok := m.Login(req, "ghost", secretPassword); ok {
		t.Fatal("expected unknown-user login failure")
	}
	if sess, ok := m.Login(req, "admin", "s3cr3t"); !ok {
		t.Fatal("expected successful login")
	} else if sess.Role != RoleOperator {
		t.Fatalf("unexpected role %q", sess.Role)
	}

	// Denied operator action with a real session cookie.
	cookie := issueTestCookie(t, m, "view")
	deniedReq := requestWithCookie(cookie)
	deniedReq.RemoteAddr = "10.9.9.9:1234"
	RequireOperator(m)(func(http.ResponseWriter, *http.Request, string) {})(httptest.NewRecorder(), deniedReq)

	output := buf.String()
	if output == "" {
		t.Fatal("expected audit output")
	}
	for _, forbidden := range []string{secretPassword, "s3cr3t", cookie.Value} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("audit output leaked a secret %q: %s", forbidden, output)
		}
	}
	if strings.Contains(strings.ToLower(output), `"password"`) || strings.Contains(strings.ToLower(output), `"token"`) {
		t.Fatalf("audit event must not have password/token fields: %s", output)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("audit line is not JSON: %v (%s)", err, line)
		}
		if ev.Action == "" || ev.Result == "" {
			t.Fatalf("audit event is missing required fields: %+v", ev)
		}
	}
	if !strings.Contains(output, ActionLogin) || !strings.Contains(output, ResultDenied) || !strings.Contains(output, ActionAuthorize) {
		t.Fatalf("expected login and authorization events, got: %s", output)
	}
}

func TestMemoryAuditorRingBuffer(t *testing.T) {
	auditor := NewMemoryAuditor(3)
	for i := 0; i < 5; i++ {
		auditor.Log(nil, AuditEvent{Action: "action-" + string(rune('a'+i)), Result: ResultOK})
	}
	events := auditor.Recent()
	if len(events) != 3 {
		t.Fatalf("expected 3 buffered events, got %d", len(events))
	}
	if events[0].Action != "action-c" || events[2].Action != "action-e" {
		t.Fatalf("unexpected ring contents: %+v", events)
	}
	if events[0].Time.IsZero() {
		t.Fatal("expected MemoryAuditor to fill the event time")
	}
	// Recent must return a copy.
	events[0].Action = "mutated"
	if got := auditor.Recent()[0].Action; got != "action-c" {
		t.Fatalf("Recent must return a copy, got %q", got)
	}
}

func TestNopAuditor(t *testing.T) {
	NopAuditor{}.Log(nil, AuditEvent{})
}

func TestJSONAuditorConcurrent(t *testing.T) {
	var buf bytes.Buffer
	auditor := NewJSONAuditor(&buf)
	const goroutines, perGoroutine = 20, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				auditor.Log(nil, AuditEvent{Actor: "user", Action: ActionPull, Result: ResultOK})
			}
		}(g)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("expected %d lines, got %d", goroutines*perGoroutine, len(lines))
	}
	for _, line := range lines {
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("interleaved output is not valid JSON: %v (%s)", err, line)
		}
	}
}

func TestManagerAuditRecordsActorAndTarget(t *testing.T) {
	auditor := NewMemoryAuditor(10)
	m := newTestManager(t, WithAuditor(auditor))
	req := requestWithCookie(issueTestCookie(t, m, "admin"))
	req.RemoteAddr = "10.7.7.7:9999"
	m.Audit(req, ActionPull, "ghcr.io/acme/api:1.0", ResultOK)
	events := auditor.Recent()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Actor != "admin" || ev.Role != string(RoleOperator) || ev.Action != ActionPull || ev.Target != "ghcr.io/acme/api:1.0" || ev.RemoteAddr != "10.7.7.7" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestTeeAuditorFansOutAndReadsBack(t *testing.T) {
	var buf bytes.Buffer
	jsonAuditor := NewJSONAuditor(&buf)
	mem := NewMemoryAuditor(10)
	tee := NewTeeAuditor(jsonAuditor, mem)
	m := newTestManager(t, WithAuditor(tee))

	if _, ok := m.Login(nil, "admin", "wrong"); ok {
		t.Fatal("expected login failure")
	}
	if _, ok := m.Login(nil, "admin", "s3cr3t"); !ok {
		t.Fatal("expected login success")
	}
	if strings.Count(strings.TrimSpace(buf.String()), "\n") != 1 {
		t.Fatalf("expected 2 JSON lines, got %q", buf.String())
	}
	recent := m.RecentAudit()
	if len(recent) != 2 {
		t.Fatalf("expected 2 buffered events, got %d: %+v", len(recent), recent)
	}
	if recent[0].Action != ActionLogin || recent[1].Result != ResultOK {
		t.Fatalf("unexpected events: %+v", recent)
	}
}

func TestManagerRecentAuditWithoutBuffer(t *testing.T) {
	var buf bytes.Buffer
	m := newTestManager(t, WithAuditor(NewJSONAuditor(&buf)))
	if got := m.RecentAudit(); got != nil {
		t.Fatalf("expected nil without a buffering auditor, got %+v", got)
	}
	if m.Auditor() == nil {
		t.Fatal("expected Auditor() to return the configured sink")
	}
}
