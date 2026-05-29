package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/shafqat-a/ai-dev-conductor/internal/session"
	"github.com/shafqat-a/ai-dev-conductor/internal/store"
)

// maxShareTTL caps how far in the future a share link may be set to expire,
// regardless of what the caller requests.
const maxShareTTL = 30 * 24 * time.Hour

// ShareTokenHash returns the sha256 of a raw share token. Only the hash is
// persisted; the raw token lives only in the URL handed to the recipient.
func ShareTokenHash(rawToken string) []byte {
	sum := sha256.Sum256([]byte(rawToken))
	return sum[:]
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HandleMintShare creates a read-only, time-boxed share link for a live session.
// The raw token is returned exactly once (in the response); only its hash is stored.
func HandleMintShare(mgr *session.Manager, publicURL string, defaultTTL time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sharing requires persistence"})
			return
		}

		id := chi.URLParam(r, "id")
		if _, ok := mgr.Get(id); !ok {
			// Sharing streams live output, so the shell must be running here.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not running"})
			return
		}

		var req struct {
			TTLSeconds int64 `json:"ttlSeconds"`
		}
		// Body is optional — defaults apply when absent or zero.
		json.NewDecoder(r.Body).Decode(&req)

		ttl := defaultTTL
		if req.TTLSeconds > 0 {
			ttl = time.Duration(req.TTLSeconds) * time.Second
		}
		if ttl > maxShareTTL {
			ttl = maxShareTTL
		}

		shareID, err := randomHex(8)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token generation failed"})
			return
		}
		rawToken, err := randomHex(32)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token generation failed"})
			return
		}

		now := time.Now()
		expiresAt := now.Add(ttl)
		if err := st.MintShare(shareID, ShareTokenHash(rawToken), id, "read", now.Unix(), expiresAt.Unix()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create share link"})
			return
		}

		path := "/s/" + rawToken
		url := path
		if publicURL != "" {
			url = publicURL + path
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"id":        shareID,
			"sessionId": id,
			"mode":      "read",
			"token":     rawToken,
			"path":      path,
			"url":       url,
			"expiresAt": expiresAt.Unix(),
		})
	}
}

// HandleListShares lists a session's share links (metadata only — never the raw token).
func HandleListShares(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		id := chi.URLParam(r, "id")
		shares, err := st.SharesForSession(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if shares == nil {
			shares = []store.ShareLink{}
		}
		writeJSON(w, http.StatusOK, shares)
	}
}

// HandleRevokeShare revokes a share link by its public id.
func HandleRevokeShare(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sharing requires persistence"})
			return
		}
		id := chi.URLParam(r, "id")
		if err := st.RevokeShare(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	}
}
