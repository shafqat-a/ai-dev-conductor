package session

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/shafqat-a/ai-dev-conductor/internal/store"
)

// flushInterval is how often live-session activity/size is persisted.
const flushInterval = 15 * time.Second

// ErrSessionLimit is returned by Create when MaxSessions is reached.
var ErrSessionLimit = fmt.Errorf("session limit reached")

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	shell    string
	dataDir  string

	store       *store.Store
	idleTimeout time.Duration // 0 = never reap
	maxSessions int           // 0 = unlimited

	stop chan struct{}
}

// NewManager wires the live-session map to the persistent metadata store. Any
// row left as "running" by a previous process is reconciled to "detached",
// since a fresh process owns no live PTYs (until session-survival lands).
//
// idleTimeout > 0 enables reaping of sessions left with no clients for longer
// than the timeout. maxSessions > 0 caps the number of concurrent live sessions.
func NewManager(shell, dataDir string, st *store.Store, idleTimeout time.Duration, maxSessions int) *Manager {
	m := &Manager{
		sessions:    make(map[string]*Session),
		shell:       shell,
		dataDir:     dataDir,
		store:       st,
		idleTimeout: idleTimeout,
		maxSessions: maxSessions,
		stop:        make(chan struct{}),
	}
	if st != nil {
		if err := st.MarkAllDetached(); err != nil {
			log.Printf("store: reconcile on startup: %v", err)
		}
		go m.flushLoop()
	}
	if idleTimeout > 0 {
		go m.reapLoop()
	}
	return m
}

func (m *Manager) Create(name string) (*Session, error) {
	if m.maxSessions > 0 {
		m.mu.RLock()
		n := len(m.sessions)
		m.mu.RUnlock()
		if n >= m.maxSessions {
			return nil, ErrSessionLimit
		}
	}

	id := uuid.New().String()[:8]

	s, err := NewSession(id, name, m.shell, m.dataDir)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	s.OnProcessExit = func(sessionID string) {
		m.mu.Lock()
		_, exists := m.sessions[sessionID]
		if exists {
			delete(m.sessions, sessionID)
		}
		m.mu.Unlock()
		if exists {
			log.Printf("session %s: auto-removed (process exited)", sessionID)
			m.setStatus(sessionID, store.StatusDead)
		}
	}
	s.OnClientsEmpty = func(sessionID string) {
		if m.store != nil {
			if err := m.store.SetClientDisconnect(sessionID, time.Now().Unix()); err != nil {
				log.Printf("store: set client-disconnect for %s: %v", sessionID, err)
			}
		}
	}
	s.OnClientsAttach = func(sessionID string) {
		// 0 marks "currently attached / active" so the reaper (M2b) ignores it.
		if m.store != nil {
			if err := m.store.SetClientDisconnect(sessionID, 0); err != nil {
				log.Printf("store: clear client-disconnect for %s: %v", sessionID, err)
			}
		}
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	if m.store != nil {
		if err := m.store.Upsert(store.SessionMeta{
			ID:                     s.ID,
			Name:                   s.GetName(),
			CreatedAt:              s.CreatedAt.Unix(),
			LastActivityAt:         s.LastActivity(),
			LastClientDisconnectAt: s.CreatedAt.Unix(), // idle from birth until first attach
			Status:                 store.StatusRunning,
		}); err != nil {
			log.Printf("store: upsert new session %s: %v", id, err)
		}
	}

	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

type SessionInfo struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	CreatedAt              string `json:"createdAt"`
	Status                 string `json:"status"`
	LastActivityAt         int64  `json:"lastActivityAt"`
	LastClientDisconnectAt int64  `json:"lastClientDisconnectAt,omitempty"`
	Cols                   int    `json:"cols,omitempty"`
	Rows                   int    `json:"rows,omitempty"`
}

const timeLayout = "2006-01-02 15:04:05"

// List returns the merged view: every persisted row, overlaid with live state
// for sessions whose shell is running in this process.
func (m *Manager) List() []SessionInfo {
	// Without a store we can only report live sessions.
	if m.store == nil {
		return m.listLiveOnly()
	}

	rows, err := m.store.List()
	if err != nil {
		log.Printf("store: list: %v", err)
		return m.listLiveOnly()
	}

	m.mu.RLock()
	live := make(map[string]*Session, len(m.sessions))
	for id, s := range m.sessions {
		live[id] = s
	}
	m.mu.RUnlock()

	list := make([]SessionInfo, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seen[r.ID] = struct{}{}
		info := SessionInfo{
			ID:                     r.ID,
			Name:                   r.Name,
			CreatedAt:              time.Unix(r.CreatedAt, 0).Format(timeLayout),
			Status:                 string(r.Status),
			LastActivityAt:         r.LastActivityAt,
			LastClientDisconnectAt: r.LastClientDisconnectAt,
			Cols:                   r.Cols,
			Rows:                   r.Rows,
		}
		if s, ok := live[r.ID]; ok {
			info.Name = s.GetName()
			info.Status = string(store.StatusRunning)
			info.LastActivityAt = s.LastActivity()
		}
		list = append(list, info)
	}
	// Include any live session not yet reflected in the store (race safety).
	for id, s := range live {
		if _, ok := seen[id]; ok {
			continue
		}
		list = append(list, SessionInfo{
			ID:             id,
			Name:           s.GetName(),
			CreatedAt:      s.CreatedAt.Format(timeLayout),
			Status:         string(store.StatusRunning),
			LastActivityAt: s.LastActivity(),
		})
	}

	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	return list
}

func (m *Manager) listLiveOnly() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, SessionInfo{
			ID:             s.ID,
			Name:           s.GetName(),
			CreatedAt:      s.CreatedAt.Format(timeLayout),
			Status:         string(store.StatusRunning),
			LastActivityAt: s.LastActivity(),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	return list
}

func (m *Manager) Rename(id, name string) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		// Allow renaming a persisted-but-detached session too.
		if m.store != nil {
			if err := m.store.SetName(id, name); err == nil {
				return nil
			}
		}
		return fmt.Errorf("session %s not found", id)
	}
	s.SetName(name)
	if m.store != nil {
		if err := m.store.SetName(id, name); err != nil {
			log.Printf("store: rename %s: %v", id, err)
		}
	}
	return nil
}

// Delete removes a session. It closes the live shell if present and always
// clears persisted metadata + history, so detached rows can be cleaned up.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	s, live := m.sessions[id]
	if live {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	persisted := false
	if m.store != nil {
		if rows, err := m.store.List(); err == nil {
			for _, r := range rows {
				if r.ID == id {
					persisted = true
					break
				}
			}
		}
	}

	if !live && !persisted {
		return fmt.Errorf("session %s not found", id)
	}

	if live {
		s.Close()
	}
	if m.store != nil {
		if err := m.store.Delete(id); err != nil {
			log.Printf("store: delete %s: %v", id, err)
		}
	}
	RemoveHistory(m.dataDir, id)
	return nil
}

func (m *Manager) CloseAll() {
	if m.stop != nil {
		select {
		case <-m.stop: // already closed
		default:
			close(m.stop)
		}
	}
	m.flush() // persist final activity before tearing down

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.Close()
		m.setStatus(id, store.StatusDetached)
		delete(m.sessions, id)
	}
}

func (m *Manager) DataDir() string {
	return m.dataDir
}

func (m *Manager) setStatus(id string, status store.Status) {
	if m.store == nil {
		return
	}
	if err := m.store.SetStatus(id, status); err != nil {
		log.Printf("store: set status %s=%s: %v", id, status, err)
	}
}

// flushLoop periodically persists live-session activity, size, and status.
func (m *Manager) flushLoop() {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.flush()
		}
	}
}

// reapInterval derives a sensible check cadence from the idle timeout.
func (m *Manager) reapInterval() time.Duration {
	d := m.idleTimeout / 2
	if d < time.Second {
		d = time.Second
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

// reapLoop periodically closes sessions left idle (no clients) past idleTimeout.
func (m *Manager) reapLoop() {
	ticker := time.NewTicker(m.reapInterval())
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.reapOnce()
		}
	}
}

// reapOnce closes and removes every live session that has had no clients for
// longer than idleTimeout.
func (m *Manager) reapOnce() {
	m.mu.RLock()
	var victims []string
	for id, s := range m.sessions {
		if d := s.IdleDuration(); d >= 0 && d > m.idleTimeout {
			victims = append(victims, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range victims {
		log.Printf("session %s: reaped (idle > %s)", id, m.idleTimeout)
		if err := m.Delete(id); err != nil {
			log.Printf("reap %s: %v", id, err)
		}
	}
}

func (m *Manager) flush() {
	if m.store == nil {
		return
	}
	m.mu.RLock()
	snapshot := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		snapshot = append(snapshot, s)
	}
	m.mu.RUnlock()

	for _, s := range snapshot {
		// Targeted updates (not a full Upsert) so we never clobber
		// last_client_disconnect_at, which is owned by the attach/detach path.
		if err := m.store.SetActivity(s.ID, s.LastActivity()); err != nil {
			log.Printf("store: flush activity %s: %v", s.ID, err)
		}
		if cols, rows := s.Size(); cols > 0 && rows > 0 {
			if err := m.store.SetSize(s.ID, int(cols), int(rows)); err != nil {
				log.Printf("store: flush size %s: %v", s.ID, err)
			}
		}
	}
}
