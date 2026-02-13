package auth

import (
	"net/http"
	"sync"
	"time"
)

const CookieName = "ai_conductor_session"

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]time.Time // token -> expiry
}

func NewSessionStore() *SessionStore {
	s := &SessionStore{
		sessions: make(map[string]time.Time),
	}
	go s.cleanup()
	return s
}

func (s *SessionStore) Add(token string, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = time.Now().Add(duration)
}

func (s *SessionStore) Validate(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	expiry, ok := s.sessions[token]
	return ok && time.Now().Before(expiry)
}

func (s *SessionStore) Remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func (s *SessionStore) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for token, expiry := range s.sessions {
			if now.After(expiry) {
				delete(s.sessions, token)
			}
		}
		s.mu.Unlock()
	}
}

func RequireAuth(store *SessionStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var token string

			// Check X-Session-Token header first (cross-origin REST requests)
			if headerToken := r.Header.Get("X-Session-Token"); headerToken != "" {
				token = headerToken
			} else if queryToken := r.URL.Query().Get("token"); queryToken != "" {
				// Query param (cross-origin WebSocket connections)
				token = queryToken
			} else if cookie, err := r.Cookie(CookieName); err == nil {
				// Cookie (local requests)
				token = cookie.Value
			}

			if token == "" || !store.Validate(token) {
				if isAPIRequest(r) {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				} else {
					http.Redirect(w, r, "/", http.StatusSeeOther)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isAPIRequest(r *http.Request) bool {
	return len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" ||
		len(r.URL.Path) >= 3 && r.URL.Path[:3] == "/ws"
}
