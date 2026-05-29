package auth

import (
	"testing"
	"time"
)

// fixedClock returns a limiter whose clock the test controls via the returned
// pointer. The cleanup goroutine only ticks every 5 minutes, so it never reads
// the clock during these fast tests.
func fixedClock(rl *RateLimiter) *time.Time {
	now := time.Unix(1_700_000_000, 0)
	rl.now = func() time.Time { return now }
	return &now
}

func TestRateLimiterLocksOutAfterMaxAttempts(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute, time.Minute)
	fixedClock(rl)

	// First two failures stay allowed.
	for i := 0; i < 2; i++ {
		rl.RecordFailure("1.2.3.4")
		if ok, _ := rl.Allowed("1.2.3.4"); !ok {
			t.Fatalf("locked out too early after %d failure(s)", i+1)
		}
	}

	// Third failure crosses the threshold.
	rl.RecordFailure("1.2.3.4")
	ok, retry := rl.Allowed("1.2.3.4")
	if ok {
		t.Fatal("expected lockout after reaching max attempts")
	}
	if retry <= 0 {
		t.Fatalf("expected positive retry-after, got %v", retry)
	}
}

func TestRateLimiterResetClearsState(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute, time.Minute)
	fixedClock(rl)

	rl.RecordFailure("5.5.5.5")
	rl.RecordFailure("5.5.5.5") // locked out
	if ok, _ := rl.Allowed("5.5.5.5"); ok {
		t.Fatal("expected lockout")
	}
	rl.Reset("5.5.5.5")
	if ok, _ := rl.Allowed("5.5.5.5"); !ok {
		t.Fatal("Reset should clear lockout")
	}
}

func TestRateLimiterLockoutExpires(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute, time.Minute)
	now := fixedClock(rl)

	rl.RecordFailure("8.8.8.8")
	rl.RecordFailure("8.8.8.8") // locked out for 1 minute
	if ok, _ := rl.Allowed("8.8.8.8"); ok {
		t.Fatal("expected lockout")
	}
	*now = now.Add(61 * time.Second)
	if ok, _ := rl.Allowed("8.8.8.8"); !ok {
		t.Fatal("lockout should have expired")
	}
}

func TestRateLimiterExponentialBackoff(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute, time.Minute) // 1 failure locks out
	now := fixedClock(rl)

	rl.RecordFailure("7.7.7.7")
	_, first := rl.Allowed("7.7.7.7")

	*now = now.Add(first + time.Second) // let first lockout expire
	rl.RecordFailure("7.7.7.7")
	_, second := rl.Allowed("7.7.7.7")

	if second <= first {
		t.Fatalf("expected second lockout (%v) longer than first (%v)", second, first)
	}
}

func TestRateLimiterPerKeyIsolation(t *testing.T) {
	rl := NewRateLimiter(2, time.Minute, time.Minute)
	fixedClock(rl)

	rl.RecordFailure("1.1.1.1")
	rl.RecordFailure("1.1.1.1") // locked
	if ok, _ := rl.Allowed("1.1.1.1"); ok {
		t.Fatal("1.1.1.1 should be locked")
	}
	if ok, _ := rl.Allowed("2.2.2.2"); !ok {
		t.Fatal("2.2.2.2 must be unaffected by another key's lockout")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	rl := NewRateLimiter(0, time.Minute, time.Minute)
	for i := 0; i < 100; i++ {
		rl.RecordFailure("x")
	}
	if ok, _ := rl.Allowed("x"); !ok {
		t.Fatal("disabled limiter must always allow")
	}
}
