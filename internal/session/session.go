package session

import (
	"log"
	"os"
	"os/exec"
	"sync"
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

	mu            sync.Mutex
	ptmx          *os.File
	cmd           *exec.Cmd
	clients       map[*Client]struct{}
	historyFile   *os.File
	done          chan struct{}
	OnProcessExit func(id string)
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
	s.clients[c] = struct{}{}
	s.mu.Unlock()
	return c
}

// RemoveClient unregisters an output consumer.
func (s *Session) RemoveClient(c *Client) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
	close(c.done)
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
	return pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
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
