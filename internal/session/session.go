package session

import (
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
)

// Client represents a connected output consumer.
type Client struct {
	ch   chan []byte
	done chan struct{}
}

type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`

	mu          sync.Mutex
	ptmx        *os.File
	cmd         *exec.Cmd
	clients     map[*Client]struct{}
	historyFile *os.File
	done        chan struct{}
	cols, rows  uint16 // last known terminal size

	// lastActivity is the unix-seconds time of the most recent PTY output.
	// Atomic so readPTY can update it on the hot path without taking mu.
	lastActivity atomic.Int64

	// clientGoneAt is the UnixNano time since which the session has had zero
	// clients (0 while at least one client is attached). Drives idle reaping.
	clientGoneAt atomic.Int64

	OnProcessExit func(id string)
	// OnClientsEmpty fires when the last attached client disconnects.
	OnClientsEmpty func(id string)
	// OnClientsAttach fires when the first client attaches (0 -> 1).
	OnClientsAttach func(id string)
}

func NewSession(id, name, shell, dataDir string) (*Session, error) {
	if name == "" {
		name = id
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	hf, err := OpenHistoryFile(dataDir, id)
	if err != nil {
		ptmx.Close()
		cmd.Process.Kill()
		return nil, err
	}

	s := &Session{
		ID:          id,
		Name:        name,
		CreatedAt:   time.Now(),
		ptmx:        ptmx,
		cmd:         cmd,
		clients:     make(map[*Client]struct{}),
		historyFile: hf,
		done:        make(chan struct{}),
	}
	s.lastActivity.Store(s.CreatedAt.Unix())
	s.clientGoneAt.Store(s.CreatedAt.UnixNano()) // idle from birth until first attach

	go s.readPTY()
	go s.waitProcess()

	return s, nil
}

func (s *Session) readPTY() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if err != nil {
			close(s.done)
			return
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		s.lastActivity.Store(time.Now().Unix())

		// Write to history
		if s.historyFile != nil {
			s.historyFile.Write(data)
		}

		// Broadcast to all clients
		s.mu.Lock()
		for c := range s.clients {
			select {
			case c.ch <- data:
			default:
				// Client too slow, skip
			}
		}
		s.mu.Unlock()
	}
}

func (s *Session) waitProcess() {
	s.cmd.Wait()
	log.Printf("session %s: shell process exited", s.ID)

	// Close the PTY so readPTY exits and clients get notified
	if s.ptmx != nil {
		s.ptmx.Close()
	}

	// Notify the manager to remove this dead session
	if s.OnProcessExit != nil {
		s.OnProcessExit(s.ID)
	}
}

// AddClient registers a new output consumer and returns it.
func (s *Session) AddClient() *Client {
	c := &Client{
		ch:   make(chan []byte, 256),
		done: make(chan struct{}),
	}
	s.mu.Lock()
	first := len(s.clients) == 0
	s.clients[c] = struct{}{}
	s.mu.Unlock()

	if first {
		s.clientGoneAt.Store(0) // active: at least one client attached
		if s.OnClientsAttach != nil {
			s.OnClientsAttach(s.ID)
		}
	}
	return c
}

// RemoveClient unregisters an output consumer.
func (s *Session) RemoveClient(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	empty := len(s.clients) == 0
	s.mu.Unlock()
	close(c.done)

	if empty {
		s.clientGoneAt.Store(time.Now().UnixNano())
		if s.OnClientsEmpty != nil {
			s.OnClientsEmpty(s.ID)
		}
	}
}

// IdleDuration reports how long the session has had zero clients. It returns
// -1 while at least one client is attached.
func (s *Session) IdleDuration() time.Duration {
	cg := s.clientGoneAt.Load()
	if cg == 0 {
		return -1
	}
	return time.Since(time.Unix(0, cg))
}

// Output returns the channel that receives PTY output for this client.
func (c *Client) Output() <-chan []byte {
	return c.ch
}

// Done returns a channel closed when the client is removed.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

func (s *Session) WriteInput(data []byte) error {
	_, err := s.ptmx.Write(data)
	return err
}

func (s *Session) Resize(rows, cols uint16) error {
	s.mu.Lock()
	s.rows, s.cols = rows, cols
	s.mu.Unlock()
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// LastActivity returns the unix-seconds time of the most recent PTY output.
func (s *Session) LastActivity() int64 {
	return s.lastActivity.Load()
}

// Size returns the last known terminal dimensions (cols, rows).
func (s *Session) Size() (cols, rows uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

func (s *Session) SessionDone() <-chan struct{} {
	return s.done
}

func (s *Session) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Name = name
}

func (s *Session) GetName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Name
}

func (s *Session) Close() {
	if s.ptmx != nil {
		s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	if s.historyFile != nil {
		s.historyFile.Close()
	}

	s.mu.Lock()
	for c := range s.clients {
		close(c.done)
	}
	s.clients = make(map[*Client]struct{})
	s.mu.Unlock()

	log.Printf("session %s closed", s.ID)
}
