package reconcile

import (
	"time"
)

// backoffEntry is the in-memory exponential backoff state for one image+node
// pair. Entries live only for the lifetime of the process; a restart clears
// them, which is acceptable because the reconciler is stateless by design.
type backoffEntry struct {
	failures    int
	next        time.Time // zero means "no pending backoff"
	lastAttempt time.Time // zero means "never attempted by this process"
	lastError   string
}

// backoffState keeps backoffEntry per image+node key.
type backoffState struct {
	entries map[string]*backoffEntry
}

func newBackoffState() *backoffState {
	return &backoffState{entries: make(map[string]*backoffEntry)}
}

func (b *backoffState) entry(k string) *backoffEntry {
	e := b.entries[k]
	if e == nil {
		e = &backoffEntry{}
		b.entries[k] = e
	}
	return e
}

// blocked reports whether an attempt for key must be deferred.
func (b *backoffState) blocked(k string, now time.Time) bool {
	e := b.entries[k]
	if e == nil || e.next.IsZero() {
		return false
	}
	return now.Before(e.next)
}

// recordFailure records a failed attempt (either TriggerPull returned an error
// or a previously attempted slot is observed as failed) and schedules the next
// attempt with exponential backoff.
func (b *backoffState) recordFailure(k string, now time.Time, cfg Config, errMsg string) {
	e := b.entry(k)
	e.failures++
	e.lastAttempt = now
	e.lastError = errMsg
	e.next = now.Add(cfg.backoffDelay(e.failures))
}

// recordAttempt records that a pull was accepted by the provider. It clears any
// pending backoff window but keeps the failure counter, so a pair that keeps
// failing after every attempt escalates until it is observed as present.
func (b *backoffState) recordAttempt(k string, now time.Time) {
	e := b.entry(k)
	e.lastAttempt = now
	e.next = time.Time{}
}

// reset drops the backoff entry, which happens when the image is observed as
// present on the node (or a pull task for the pair is observed as succeeded).
func (b *backoffState) reset(k string) {
	delete(b.entries, k)
}

// failures returns the consecutive failure count for key (0 when unknown).
func (b *backoffState) failures(k string) int {
	if e := b.entries[k]; e != nil {
		return e.failures
	}
	return 0
}

// prune drops stale entries to keep the in-memory map bounded: a long-expired
// backoff window or an idle successful attempt is forgotten.
func (b *backoffState) prune(now time.Time, ttl time.Duration) {
	for k, e := range b.entries {
		if e.next.IsZero() {
			if !e.lastAttempt.IsZero() && now.Sub(e.lastAttempt) > ttl {
				delete(b.entries, k)
			}
			continue
		}
		if e.next.Add(ttl).Before(now) {
			delete(b.entries, k)
		}
	}
}
