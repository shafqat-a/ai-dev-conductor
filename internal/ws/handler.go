package ws

import (
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
		go readPump(conn, sess, client)
	}
}

func readPump(conn *websocket.Conn, sess *session.Session, client *session.Client) {
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
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
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
