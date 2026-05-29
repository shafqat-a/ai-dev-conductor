package auth

import (
	"net/http"
	"strings"
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

// RequireAuth gates routes behind a valid session. basePath is the URL prefix the
// app is mounted under (e.g. "/terminaltest", or "" for root); it's needed so the
// unauthenticated redirect lands on the login page and so API/WS paths are detected
// correctly when the prefix is present.
func RequireAuth(store *SessionStore, basePath string) func(http.Handler) http.Handler {
	loginURL := basePath + "/"
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
				if isAPIRequest(r, basePath) {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				} else {
					http.Redirect(w, r, loginURL, http.StatusSeeOther)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isAPIRequest(r *http.Request, basePath string) bool {
	path := strings.TrimPrefix(r.URL.Path, basePath)
	return strings.HasPrefix(path, "/api") || strings.HasPrefix(path, "/ws")
}
