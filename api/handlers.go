package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/shafqat-a/ai-dev-conductor/internal/auth"
	"github.com/shafqat-a/ai-dev-conductor/internal/session"
)

func HandleHealthCheck() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func HandleLogin(authSvc *auth.AuthService, store *auth.SessionStore, limiter *auth.RateLimiter, sessionTimeout time.Duration, basePath string) http.HandlerFunc {
	cookiePath := basePath + "/"
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)

		// Reject early if this client is locked out from too many failures.
		if ok, retryAfter := limiter.Allowed(ip); !ok {
			secs := int(retryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			log.Printf("auth: login throttled, ip=%s retry_after=%ds", ip, secs)
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts, try again later"})
			return
		}

		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}

		if !authSvc.VerifyPassword(req.Password) {
			limiter.RecordFailure(ip)
			// Structured, fail2ban-friendly line.
			log.Printf("auth: failed login attempt, ip=%s", ip)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
			return
		}

		// Success — clear any accumulated failure state for this client.
		limiter.Reset(ip)

		token, err := auth.GenerateSessionToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		store.Add(token, sessionTimeout)

		http.SetCookie(w, &http.Cookie{
			Name:     auth.CookieName,
			Value:    token,
			Path:     cookiePath,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(sessionTimeout.Seconds()),
		})

		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "token": token})
	}
}

func HandleListSessions(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mgr.List())
	}
}

func HandleCreateSession(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		// Body is optional — name defaults to ID if empty
		json.NewDecoder(r.Body).Decode(&req)

		s, err := mgr.Create(req.Name)
		if errors.Is(err, session.ErrSessionLimit) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"id": s.ID, "name": s.Name})
	}
}

func HandleRenameSession(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		if err := mgr.Rename(id, req.Name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	}
}

func HandleDeleteSession(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := mgr.Delete(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	}
}

// HandleUpload writes an uploaded file into the session's current working
// directory. The filename is reduced to its base name to block path traversal,
// and the request body is capped at maxBytes (mirror this in nginx's
// client_max_body_size when behind a proxy).
func HandleUpload(mgr *session.Manager, maxBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cwd, ok := sessionCWD(w, r, mgr)
		if !ok {
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "upload too large or malformed"})
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field"})
			return
		}
		defer file.Close()

		// Collapse to the base name so "../" or absolute paths in the client-supplied
		// filename can never escape the working directory.
		name := filepath.Base(filepath.Clean("/" + hdr.Filename))
		if name == "." || name == "/" || strings.TrimSpace(name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid filename"})
			return
		}
		dest := filepath.Join(cwd, name)

		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot create file"})
			return
		}
		defer out.Close()

		n, err := io.Copy(out, file)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"name": name, "size": n})
	}
}

// HandleDownload serves a file from within the session's working directory. The
// requested ?path= is confined to the CWD: traversal outside it is rejected.
func HandleDownload(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cwd, ok := sessionCWD(w, r, mgr)
		if !ok {
			return
		}
		rel := r.URL.Query().Get("path")
		if rel == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
			return
		}
		full, err := confinedPath(cwd, rel)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "path outside working directory"})
			return
		}
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
			return
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(full)))
		http.ServeFile(w, r, full)
	}
}

// sessionCWD looks up the session named in the {id} route param and returns its
// working directory, writing the appropriate error response and returning ok=false
// if the session is missing or its CWD cannot be resolved.
func sessionCWD(w http.ResponseWriter, r *http.Request, mgr *session.Manager) (string, bool) {
	id := chi.URLParam(r, "id")
	s, ok := mgr.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return "", false
	}
	cwd, err := s.CWD()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "cannot resolve working directory"})
		return "", false
	}
	return cwd, true
}

// confinedPath joins rel onto base and guarantees the result stays within base,
// defeating "../" traversal and absolute-path escapes.
func confinedPath(base, rel string) (string, error) {
	full := filepath.Join(base, rel)
	r, err := filepath.Rel(base, full)
	if err != nil {
		return "", err
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes base directory")
	}
	return full, nil
}

// clientIP resolves the originating client address. The app sits behind our
// own nginx, so trust the first hop in X-Forwarded-For when present; otherwise
// fall back to the connection's RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
