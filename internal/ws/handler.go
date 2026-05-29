package ws

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/shafqat-a/ai-dev-conductor/internal/session"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

const (
	pingInterval = 30 * time.Second
	pongWait     = 60 * time.Second
	writeWait    = 10 * time.Second
)

func HandleWebSocket(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		sess, ok := mgr.Get(id)
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade: %v", err)
			return
		}

		client := sess.AddClient()
		log.Printf("client connected to session %s", id)

		// Send history on connect
		history, err := session.ReadHistory(mgr.DataDir(), id)
		if err == nil && len(history) > 0 {
			msg := Message{Type: MessageTypeOutput, Data: string(history)}
			if payload, err := json.Marshal(msg); err == nil {
				conn.WriteMessage(websocket.TextMessage, payload)
			}
		}

		go writePump(conn, client)
		go readPump(conn, sess, client, mgr.DataDir(), false)
	}
}

// HandleShareWebSocket is the public, unauthenticated attach endpoint for a
// read-only share link. The raw token comes from the URL; we resolve it to a
// live session via the store. The connection is read-only: input/resize/paste
// frames are dropped server-side (UI-only read-only would be a security hole).
func HandleShareWebSocket(mgr *session.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := mgr.Store()
		if st == nil {
			http.Error(w, "sharing unavailable", http.StatusServiceUnavailable)
			return
		}

		token := chi.URLParam(r, "token")
		sum := sha256.Sum256([]byte(token))
		sessionID, _, ok, err := st.RedeemShare(sum[:], time.Now().Unix())
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "share link invalid or expired", http.StatusNotFound)
			return
		}

		sess, live := mgr.Get(sessionID)
		if !live {
			http.Error(w, "session not running", http.StatusNotFound)
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("websocket upgrade (share): %v", err)
			return
		}

		client := sess.AddClient()
		log.Printf("read-only viewer connected to session %s", sessionID)

		history, err := session.ReadHistory(mgr.DataDir(), sessionID)
		if err == nil && len(history) > 0 {
			msg := Message{Type: MessageTypeOutput, Data: string(history)}
			if payload, err := json.Marshal(msg); err == nil {
				conn.WriteMessage(websocket.TextMessage, payload)
			}
		}

		go writePump(conn, client)
		go readPump(conn, sess, client, mgr.DataDir(), true)
	}
}

// readPump pumps client→server frames. When readOnly is true the session is
// never written to: input, resize, and paste frames are ignored (the connection
// is still read so we observe pongs and the close handshake).
func readPump(conn *websocket.Conn, sess *session.Session, client *session.Client, dataDir string, readOnly bool) {
	defer func() {
		sess.RemoveClient(client)
		conn.Close()
		log.Printf("client disconnected from session %s", sess.ID)
	}()

	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		msgType, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}

		// Read-only viewers (share links) may not drive the PTY. We still
		// consume their frames so pongs and the close handshake are handled.
		if readOnly {
			continue
		}

		// Binary messages are raw PTY input (e.g. image paste)
		if msgType == websocket.BinaryMessage {
			sess.WriteInput(raw)
			continue
		}

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case MessageTypeInput:
			sess.WriteInput([]byte(msg.Data))
		case MessageTypeResize:
			if msg.Cols > 0 && msg.Rows > 0 {
				sess.Resize(msg.Rows, msg.Cols)
			}
		case MessageTypePasteImage:
			img, err := base64.StdEncoding.DecodeString(msg.Data)
			if err != nil {
				log.Printf("session %s: bad paste-image data: %v", sess.ID, err)
				continue
			}
			if err := sess.PasteImage(img, msg.Mime, dataDir); err != nil {
				log.Printf("session %s: paste image failed: %v", sess.ID, err)
			}
		}
	}
}

func writePump(conn *websocket.Conn, client *session.Client) {
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		conn.Close()
	}()

	for {
		select {
		case data, ok := <-client.Output():
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			msg := Message{Type: MessageTypeOutput, Data: string(data)}
			payload, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-client.Done():
			return
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
