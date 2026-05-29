package session

import (
	"bytes"
	"fmt"
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
	ch        chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// closeDone closes the done channel exactly once, making it safe to call from
// both RemoveClient (per-connection cleanup) and Session.Close (session teardown).
func (c *Client) closeDone() {
	c.closeOnce.Do(func() { close(c.done) })
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

	// tmux-backed sessions wrap a detached tmux session so the shell survives a
	// conductor restart. tmuxName is the tmux session name; dataDir locates both
	// the history file (raw-PTY mode) and the tmux server socket (tmux mode).
	tmux     bool
	tmuxName string
	dataDir  string

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

// NewSession starts a fresh session. When useTmux is true the shell runs inside a
// detached tmux session (surviving a conductor restart); otherwise it is a plain
// PTY owned by this process.
func NewSession(id, name, shell, dataDir string, useTmux bool) (*Session, error) {
	return newSession(id, name, shell, dataDir, useTmux, time.Now())
}

// ReattachSession re-binds to an already-running tmux session after a restart,
// preserving its original creation time. The tmux `new-session -A` is idempotent,
// so this simply attaches to the surviving shell with its full scrollback intact.
func ReattachSession(id, name, shell, dataDir string, createdAt time.Time) (*Session, error) {
	return newSession(id, name, shell, dataDir, true, createdAt)
}

func newSession(id, name, shell, dataDir string, useTmux bool, createdAt time.Time) (*Session, error) {
	if name == "" {
		name = id
	}

	var (
		cmd    *exec.Cmd
		hf     *os.File
		tmuxNm string
		err    error
	)
	if useTmux {
		tmuxNm = tmuxName(id)
		cmd = tmuxAttachCmd(dataDir, tmuxNm, shell)
	} else {
		cmd = exec.Command(shell)
		cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	// tmux owns scrollback (served via capture-pane on attach); only raw-PTY
	// sessions need a history file, since they have nowhere else to replay from.
	if !useTmux {
		hf, err = OpenHistoryFile(dataDir, id)
		if err != nil {
			ptmx.Close()
			cmd.Process.Kill()
			return nil, err
		}
	}

	s := &Session{
		ID:          id,
		Name:        name,
		CreatedAt:   createdAt,
		ptmx:        ptmx,
		cmd:         cmd,
		clients:     make(map[*Client]struct{}),
		historyFile: hf,
		done:        make(chan struct{}),
		tmux:        useTmux,
		tmuxName:    tmuxNm,
		dataDir:     dataDir,
	}
	s.lastActivity.Store(createdAt.Unix())
	s.clientGoneAt.Store(time.Now().UnixNano()) // idle from birth until first attach

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
	c.closeDone()

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

// Snapshot returns the output to seed a freshly-connected viewer: the tmux pane's
// recent scrollback (CRLF-normalized) for tmux sessions, or the on-disk history
// file for raw-PTY sessions.
func (s *Session) Snapshot() []byte {
	if s.tmux {
		raw := tmuxCapturePane(s.dataDir, s.tmuxName, captureLines)
		// capture-pane emits bare LFs; xterm needs CRLF or lines stair-step.
		return bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
	}
	data, _ := ReadHistory(s.dataDir, s.ID)
	return data
}

// CWD returns the working directory of the session's shell, resolved via
// /proc/<pid>/cwd (Linux only). For tmux sessions the shell is the tmux pane's
// process; for raw-PTY sessions it is the directly-spawned shell. It reflects the
// shell's current directory, so it tracks `cd` as the user navigates.
func (s *Session) CWD() (string, error) {
	pid, err := s.shellPID()
	if err != nil {
		return "", err
	}
	return os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
}

// shellPID returns the PID of the interactive shell backing this session.
func (s *Session) shellPID() (int, error) {
	if s.tmux {
		return tmuxPanePID(s.dataDir, s.tmuxName)
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return 0, fmt.Errorf("session %s: no live process", s.ID)
	}
	return s.cmd.Process.Pid, nil
}

// Detach releases this process's hold on the session without terminating the
// shell. For tmux sessions the tmux server keeps the shell (and scrollback) alive
// for reattachment after a restart. Raw-PTY sessions have nothing to preserve, so
// this is equivalent to Close.
func (s *Session) Detach() {
	if !s.tmux {
		s.Close()
		return
	}
	if s.ptmx != nil {
		s.ptmx.Close() // detaching the client; the tmux session lives on
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.mu.Lock()
	for c := range s.clients {
		c.closeDone()
	}
	s.clients = make(map[*Client]struct{})
	s.mu.Unlock()
	log.Printf("session %s detached (tmux session preserved)", s.ID)
}

func (s *Session) Close() {
	if s.ptmx != nil {
		s.ptmx.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	if s.tmux {
		if err := tmuxKillSession(s.dataDir, s.tmuxName); err != nil {
			log.Printf("session %s: kill tmux session: %v", s.ID, err)
		}
	}
	if s.historyFile != nil {
		s.historyFile.Close()
	}

	s.mu.Lock()
	for c := range s.clients {
		c.closeDone()
	}
	s.clients = make(map[*Client]struct{})
	s.mu.Unlock()

	log.Printf("session %s closed", s.ID)
}
