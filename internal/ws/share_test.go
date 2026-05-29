package ws

import (
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/shafqat-a/ai-dev-conductor/internal/session"
	"github.com/shafqat-a/ai-dev-conductor/internal/store"
)

// drain reads frames until the deadline and returns all concatenated output data.
func drain(t *testing.T, c *websocket.Conn, d time.Duration) string {
	t.Helper()
	var sb strings.Builder
	c.SetReadDeadline(time.Now().Add(d))
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			return sb.String()
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if m.Type == MessageTypeOutput {
			sb.WriteString(m.Data)
		}
	}
}

func TestShareWebSocketIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mgr := session.NewManager("/bin/sh", dir, st, 0, 0, false)
	defer mgr.CloseAll() // stop session goroutines before the store closes
	sess, err := mgr.Create("")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	token := "readonly-test-token"
	sum := sha256.Sum256([]byte(token))
	now := time.Now().Unix()
	if err := st.MintShare("sh1", sum[:], sess.ID, "read", now, now+3600); err != nil {
		t.Fatalf("MintShare: %v", err)
	}

	r := chi.NewRouter()
	r.Get("/ws/{id}", HandleWebSocket(mgr))
	r.Get("/ws/share/{token}", HandleShareWebSocket(mgr))
	srv := httptest.NewServer(r)
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	owner, _, err := websocket.DefaultDialer.Dial(base+"/ws/"+sess.ID, nil)
	if err != nil {
		t.Fatalf("owner dial: %v", err)
	}
	defer owner.Close()

	viewer, _, err := websocket.DefaultDialer.Dial(base+"/ws/share/"+token, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	defer viewer.Close()

	// Let both connections settle.
	time.Sleep(200 * time.Millisecond)

	// The viewer attempts to inject a command — this MUST be ignored server-side.
	if err := viewer.WriteJSON(Message{Type: MessageTypeInput, Data: "echo VIEWER_INJECTED\n"}); err != nil {
		t.Fatalf("viewer write: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// The owner runs a legitimate command.
	if err := owner.WriteJSON(Message{Type: MessageTypeInput, Data: "echo OWNER_RAN\n"}); err != nil {
		t.Fatalf("owner write: %v", err)
	}

	ownerOut := drain(t, owner, 1500*time.Millisecond)
	viewerOut := drain(t, viewer, 500*time.Millisecond)

	if !strings.Contains(ownerOut, "OWNER_RAN") {
		t.Fatalf("owner command did not run; output=%q", ownerOut)
	}
	if strings.Contains(ownerOut, "VIEWER_INJECTED") {
		t.Fatalf("read-only viewer injected input into the PTY; output=%q", ownerOut)
	}
	// Read path works: the viewer sees the live stream.
	if !strings.Contains(viewerOut, "OWNER_RAN") {
		t.Fatalf("viewer did not receive live output; output=%q", viewerOut)
	}
}

func TestShareWebSocketRejectsBadToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	mgr := session.NewManager("/bin/sh", dir, st, 0, 0, false)

	r := chi.NewRouter()
	r.Get("/ws/share/{token}", HandleShareWebSocket(mgr))
	srv := httptest.NewServer(r)
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	_, resp, err := websocket.DefaultDialer.Dial(base+"/ws/share/nonexistent", nil)
	if err == nil {
		t.Fatal("expected dial to fail for unknown token")
	}
	if resp == nil || resp.StatusCode != 404 {
		t.Fatalf("want 404 for unknown token, got %v", resp)
	}
}
