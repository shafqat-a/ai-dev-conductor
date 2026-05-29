package auth

import (
	"sync"
	"time"
)

// RateLimiter throttles repeated failed login attempts per client key (IP).
//
// It counts failures within a sliding window; once a key reaches maxAttempts
// failures it is locked out for an exponentially increasing duration (base,
// 2*base, 4*base, …) capped at 16*base. A successful login (Reset) clears all
// state for that key. A non-positive maxAttempts disables the limiter entirely.
type RateLimiter struct {
	mu      sync.Mutex
	entries map[string]*rlEntry

	maxAttempts int
	window      time.Duration
	baseLockout time.Duration
	maxLockout  time.Duration

	// now is injectable so tests can advance time without sleeping.
	now func() time.Time
}

type rlEntry struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
	level       int // backoff level; drives exponential lockout
	lastSeen    time.Time
}

// NewRateLimiter builds a limiter. If maxAttempts <= 0 the limiter is a no-op.
func NewRateLimiter(maxAttempts int, window, baseLockout time.Duration) *RateLimiter {
	rl := &RateLimiter{
		entries:     make(map[string]*rlEntry),
		maxAttempts: maxAttempts,
		window:      window,
		baseLockout: baseLockout,
		maxLockout:  baseLockout * 16,
		now:         time.Now,
	}
	if rl.enabled() {
		go rl.cleanup()
	}
	return rl
}

func (rl *RateLimiter) enabled() bool { return rl.maxAttempts > 0 }

// Allowed reports whether a login attempt from key may proceed. When locked
// out it returns false and the duration the caller should report in Retry-After.
func (rl *RateLimiter) Allowed(key string) (bool, time.Duration) {
	if !rl.enabled() {
		return true, 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	e := rl.entries[key]
	if e == nil {
		return true, 0
	}
	now := rl.now()
	if now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	return true, 0
}

// RecordFailure registers one failed attempt for key, opening or extending a
// lockout once the failure threshold is hit within the window.
func (rl *RateLimiter) RecordFailure(key string) {
	if !rl.enabled() {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	e := rl.entries[key]
	if e == nil {
		e = &rlEntry{}
		rl.entries[key] = e
	}
	e.lastSeen = now

	// Still locked out — don't accumulate further; the lockout already stands.
	if now.Before(e.lockedUntil) {
		return
	}

	if e.windowStart.IsZero() || now.Sub(e.windowStart) > rl.window {
		e.windowStart = now
		e.failures = 0
	}
	e.failures++

	if e.failures >= rl.maxAttempts {
		e.level++
		d := rl.baseLockout << (e.level - 1)
		if d > rl.maxLockout || d <= 0 { // d<=0 guards shift overflow
			d = rl.maxLockout
		}
		e.lockedUntil = now.Add(d)
		// Reset the window so post-lockout attempts start fresh, but keep
		// level so repeat offenders are locked out for longer.
		e.failures = 0
		e.windowStart = time.Time{}
	}
}

// Reset clears all throttling state for key (call on successful login).
func (rl *RateLimiter) Reset(key string) {
	if !rl.enabled() {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	delete(rl.entries, key)
}

// cleanup periodically drops entries that are no longer locked and have been
// idle longer than the retention horizon, so the map can't grow unbounded.
func (rl *RateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := rl.now()
		retention := rl.maxLockout
		if rl.window > retention {
			retention = rl.window
		}
		for key, e := range rl.entries {
			if now.After(e.lockedUntil) && now.Sub(e.lastSeen) > retention {
				delete(rl.entries, key)
			}
		}
		rl.mu.Unlock()
	}
}
