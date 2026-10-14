package auth

import (
	"sync"
	"time"
)

// FailureLimiter locks a key out after max failures within window.
// State is in memory, which is correct for the single-replica deployment.
type FailureLimiter struct {
	max    int
	window time.Duration

	mu      sync.Mutex
	entries map[string]*failures
}

type failures struct {
	count    int
	inflight int // attempts Acquired but not yet Released
	reset    time.Time
}

func NewFailureLimiter(max int, window time.Duration) *FailureLimiter {
	return &FailureLimiter{max: max, window: window, entries: map[string]*failures{}}
}

// Allowed reports whether key may make another attempt. Attempts that
// are in flight (Acquired, not yet Released) count towards the limit, so
// a burst of concurrent attempts cannot all pass the check before any of
// them has failed.
func (l *FailureLimiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return true
	}
	if time.Now().After(e.reset) && e.inflight == 0 {
		delete(l.entries, key)
		return true
	}
	return e.count+e.inflight < l.max
}

// Acquire reserves one attempt for key and reports whether it may proceed.
// Every successful Acquire must be paired with Release.
func (l *FailureLimiter) Acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.entries[key]
	if ok && now.After(e.reset) && e.inflight == 0 {
		delete(l.entries, key)
		ok = false
	}
	if ok && e.count+e.inflight >= l.max {
		return false
	}
	if !ok {
		e = &failures{reset: now.Add(l.window)}
		l.entries[key] = e
	}
	e.inflight++
	return true
}

// Release ends an attempt Acquired earlier; a failed attempt counts
// towards the limit. Releasing a key that was Reset meanwhile is a no-op.
func (l *FailureLimiter) Release(key string, failed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		if !failed {
			return
		}
		e = &failures{reset: time.Now().Add(l.window)}
		l.entries[key] = e
	}
	if e.inflight > 0 {
		e.inflight--
	}
	if failed {
		e.count++
		if len(l.entries) > 10_000 {
			l.sweep(time.Now())
		}
	}
}

func (l *FailureLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.entries[key]
	if !ok || now.After(e.reset) {
		e = &failures{reset: now.Add(l.window)}
		l.entries[key] = e
	}
	e.count++
	if len(l.entries) > 10_000 {
		l.sweep(now)
	}
}

func (l *FailureLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func (l *FailureLimiter) sweep(now time.Time) {
	for k, e := range l.entries {
		if now.After(e.reset) && e.inflight == 0 {
			delete(l.entries, k)
		}
	}
}
