package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// maxExecOutput caps the bytes returned by the exec/history endpoints.
const maxExecOutput = 500 * 1024

func HandleHealthCheck() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func HandleLogin(authSvc *auth.AuthService, store *auth.SessionStore, limiter *auth.RateLimiter, sessionTimeout time.Duration) http.HandlerFunc {
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
			Path:     "/",
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

// HandleExecCommand runs a single command in a session's shell and returns its
// output. It writes the command to the PTY followed by a unique done-marker,
// then tails the session history file with its OWN independent reader until the
// marker appears (or the timeout elapses). It never touches the PTY read loop,
// so it works even with no WebSocket client attached.
func HandleExecCommand(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		s, ok := mgr.Get(id)
		if !ok {
			// Session exists on disk but isn't live (e.g. detached after a
			// restart): there's no PTY to write to, so exec can't run.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not running"})
			return
		}

		var req struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if strings.TrimSpace(req.Command) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command is required"})
			return
		}

		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 30
		}
		if timeout > 120 {
			timeout = 120
		}

		// Unique marker so concurrent execs (or stale output) can't be confused.
		nonce, err := randomNonce()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		marker := "__HERMES_DONE_" + nonce + "__"

		// Open our OWN read-only handle on the history file and seek to the end
		// before writing anything, so we only see output produced by this command.
		logPath := filepath.Join(mgr.DataDir(), id+".log")
		f, err := os.Open(logPath)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "open history: " + err.Error()})
			return
		}
		defer f.Close()
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "seek history: " + err.Error()})
			return
		}

		// Clear any partial input, then run the command and emit the marker.
		if err := s.WriteInput([]byte("\n")); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write input: " + err.Error()})
			return
		}
		if err := s.WriteInput([]byte(req.Command + "; echo " + marker + "\n")); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write input: " + err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()

		resCh := make(chan execResult, 1)
		go pollExecOutput(ctx, f, marker, resCh)
		res := <-resCh

		output := res.output
		truncated := 0
		if len(output) > maxExecOutput {
			truncated = len(output) - maxExecOutput
			output = output[:maxExecOutput]
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"output":          string(output),
			"timeout":         res.timedOut,
			"truncated_bytes": truncated,
		})
	}
}

type execResult struct {
	output   []byte
	timedOut bool
}

// pollExecOutput tails f (already seeked to the start offset) on 50ms intervals,
// accumulating new bytes until the done-marker appears or ctx expires. On either
// outcome it sends the extracted command output (marker line stripped).
func pollExecOutput(ctx context.Context, f *os.File, marker string, ch chan<- execResult) {
	var buf []byte
	tmp := make([]byte, 32*1024)
	for {
		// Drain everything currently available.
		for {
			n, err := f.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if n == 0 || err != nil {
				break
			}
		}

		if out, done := extractExecOutput(buf, marker); done {
			ch <- execResult{output: out}
			return
		}

		select {
		case <-ctx.Done():
			out, _ := extractExecOutput(buf, marker)
			ch <- execResult{output: out, timedOut: true}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// extractExecOutput returns the command output captured in buf and whether the
// done-marker has appeared as actual program output.
//
// The marker shows up TWICE in PTY history: first in the echoed input line
// ("...; echo <marker>"), then on its own line as the echo's output. We skip the
// echoed occurrence (preceded by "echo ") and treat the next one as completion.
func extractExecOutput(buf []byte, marker string) (output []byte, done bool) {
	m := []byte(marker)
	echoTag := append([]byte("echo "), m...)

	// Strip the echoed command line so it isn't returned as output.
	contentStart := 0
	if echoIdx := bytes.Index(buf, echoTag); echoIdx >= 0 {
		if nl := bytes.IndexByte(buf[echoIdx:], '\n'); nl >= 0 {
			contentStart = echoIdx + nl + 1
		}
	}
	rest := buf[contentStart:]

	// Find the marker as real output (not the "echo <marker>" echo).
	for off := 0; off <= len(rest)-len(m); {
		i := bytes.Index(rest[off:], m)
		if i < 0 {
			break
		}
		abs := off + i
		if abs >= 5 && bytes.Equal(rest[abs-5:abs], []byte("echo ")) {
			off = abs + len(m)
			continue
		}
		out := bytes.TrimRight(rest[:abs], "\r\n")
		return out, true
	}

	// Not done yet — return what we have so far (used on timeout).
	return rest, false
}

// HandleSessionHistory returns the last N bytes of a session's log file without
// sending any input — a read-only peek at what the session is currently doing.
func HandleSessionHistory(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		tail := 5000
		if t := r.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.Atoi(t); err == nil && n > 0 {
				tail = n
			}
		}
		if tail > 500000 {
			tail = 500000
		}

		logPath := filepath.Join(mgr.DataDir(), id+".log")
		data, err := tailFile(logPath, int64(tail))
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read history: " + err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"session_id": id,
			"output":     string(data),
		})
	}
}

// tailFile returns the last n bytes of the file at path.
func tailFile(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := info.Size() - n
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// randomNonce returns 8 random bytes hex-encoded, for the exec done-marker.
func randomNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
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
