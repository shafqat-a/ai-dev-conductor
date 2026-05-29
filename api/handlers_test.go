package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shafqat-a/ai-dev-conductor/internal/auth"
)

func newLoginHandler(t *testing.T, maxAttempts int) http.HandlerFunc {
	t.Helper()
	authSvc, err := auth.NewAuthService("correct-horse")
	if err != nil {
		t.Fatalf("NewAuthService: %v", err)
	}
	store := auth.NewSessionStore()
	limiter := auth.NewRateLimiter(maxAttempts, time.Minute, time.Minute)
	return HandleLogin(authSvc, store, limiter, time.Hour, "")
}

func postLogin(h http.HandlerFunc, ip, password string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"`+password+`"}`))
	req.RemoteAddr = ip + ":54321"
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestHandleLoginThrottlesAfterFailures(t *testing.T) {
	h := newLoginHandler(t, 3)

	// 3 wrong attempts each get 401.
	for i := 0; i < 3; i++ {
		if rr := postLogin(h, "9.9.9.9", "wrong"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, rr.Code)
		}
	}
	// 4th is throttled.
	rr := postLogin(h, "9.9.9.9", "wrong")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after lockout, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

func TestHandleLoginSuccessResetsCounter(t *testing.T) {
	h := newLoginHandler(t, 3)

	postLogin(h, "4.4.4.4", "wrong")
	postLogin(h, "4.4.4.4", "wrong")

	// Correct password before lockout succeeds and clears the counter.
	if rr := postLogin(h, "4.4.4.4", "correct-horse"); rr.Code != http.StatusOK {
		t.Fatalf("want 200 on correct password, got %d", rr.Code)
	}
	// Fresh failures should not be immediately locked out.
	if rr := postLogin(h, "4.4.4.4", "wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("counter not reset after success: got %d", rr.Code)
	}
}

func TestHandleLoginThrottleIsPerIP(t *testing.T) {
	h := newLoginHandler(t, 2)

	postLogin(h, "1.1.1.1", "wrong")
	postLogin(h, "1.1.1.1", "wrong") // 1.1.1.1 now locked

	if rr := postLogin(h, "1.1.1.1", "wrong"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("1.1.1.1 should be locked, got %d", rr.Code)
	}
	if rr := postLogin(h, "2.2.2.2", "wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("2.2.2.2 must be unaffected, got %d", rr.Code)
	}
}

func TestClientIPPrefersForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	if got := clientIP(req); got != "203.0.113.7" {
		t.Fatalf("want first XFF hop 203.0.113.7, got %q", got)
	}
}
