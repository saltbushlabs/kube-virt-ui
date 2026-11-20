package auth

import (
	"container/list"
	"sync"
	"time"
)

const defaultMaxEntries = 10_000

// FailureLimiter locks a key out after max failures within window.
// State is in memory, which is correct for the single-replica deployment.
type FailureLimiter struct {
	max           int
	window        time.Duration
	maxEntries    int
	sweepInterval time.Duration
	now           func() time.Time

	mu        sync.Mutex
	entries   map[string]*failures
	order     list.List // *failures ordered by reset time (oldest at front)
	lastSweep time.Time
}

type failures struct {
	key      string
	count    int
	inflight int // attempts Acquired but not yet Released
	reset    time.Time
	elem     *list.Element
}

func NewFailureLimiter(max int, window time.Duration) *FailureLimiter {
	return &FailureLimiter{
		max:           max,
		window:        window,
		maxEntries:    defaultMaxEntries,
		sweepInterval: min(window, time.Minute),
		now:           time.Now,
		entries:       map[string]*failures{},
	}
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
	if l.now().After(e.reset) && e.inflight == 0 {
		l.deleteEntry(e)
		return true
	}
	return e.count+e.inflight < l.max
}

// Acquire reserves one attempt for key and reports whether it may proceed.
// Every successful Acquire must be paired with Release.
func (l *FailureLimiter) Acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if ok && now.After(e.reset) && e.inflight == 0 {
		l.deleteEntry(e)
		ok = false
	}
	if ok && e.count+e.inflight >= l.max {
		return false
	}
	if !ok {
		e = l.insertEntry(key, now)
		if e == nil {
			return false
		}
	}
	e.inflight++
	return true
}

// Release ends an attempt Acquired earlier; a failed attempt counts
// towards the limit. An entry left with nothing to remember (no failures,
// nothing in flight — the successful-login case) is dropped so the map
// only ever holds keys with failures or live attempts.
func (l *FailureLimiter) Release(key string, failed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if !ok {
		if !failed {
			return
		}
		e = l.insertEntry(key, now)
		if e == nil {
			return
		}
	}
	if e.inflight > 0 {
		e.inflight--
	}
	if failed {
		if now.After(e.reset) {
			e.count = 0
			e.reset = now.Add(l.window)
			l.order.MoveToBack(e.elem)
		}
		e.count++
		l.maybeSweep(now)
		return
	}
	if e.count == 0 && e.inflight == 0 {
		l.deleteEntry(e)
	}
}

func (l *FailureLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if ok && now.After(e.reset) {
		if e.inflight == 0 {
			l.deleteEntry(e)
			ok = false
		} else {
			e.count = 0
			e.reset = now.Add(l.window)
			l.order.MoveToBack(e.elem)
		}
	}
	if !ok {
		e = l.insertEntry(key, now)
		if e == nil {
			return
		}
	}
	e.count++
	l.maybeSweep(now)
}

// Reset clears a key's failure count (after a successful login). Attempts
// still in flight keep counting towards the limit; their Release drops
// the entry once the last one finishes without failing.
func (l *FailureLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return
	}
	if e.inflight == 0 {
		l.deleteEntry(e)
		return
	}
	e.count = 0
}

func (l *FailureLimiter) insertEntry(key string, now time.Time) *failures {
	l.maybeSweep(now)
	for l.maxEntries > 0 && len(l.entries) >= l.maxEntries {
		if !l.evictOldest() {
			return nil
		}
	}
	e := &failures{key: key, reset: now.Add(l.window)}
	e.elem = l.order.PushBack(e)
	l.entries[key] = e
	return e
}

func (l *FailureLimiter) deleteEntry(e *failures) {
	delete(l.entries, e.key)
	if e.elem != nil {
		l.order.Remove(e.elem)
		e.elem = nil
	}
}

func (l *FailureLimiter) evictOldest() bool {
	for el := l.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*failures)
		if e.inflight == 0 {
			l.deleteEntry(e)
			return true
		}
	}
	return false
}

func (l *FailureLimiter) maybeSweep(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < l.sweepInterval {
		return
	}
	l.sweep(now)
}

func (l *FailureLimiter) sweep(now time.Time) {
	for el := l.order.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*failures)
		if !now.After(e.reset) {
			break
		}
		if e.inflight == 0 {
			l.deleteEntry(e)
		}
		el = next
	}
	l.lastSweep = now
}
