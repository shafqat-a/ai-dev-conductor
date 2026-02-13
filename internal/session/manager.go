package session

import (
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/google/uuid"
)

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	shell    string
	dataDir  string
}

func NewManager(shell, dataDir string) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		shell:    shell,
		dataDir:  dataDir,
	}
}

func (m *Manager) Create(name string) (*Session, error) {
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
		}
	}

	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	return s, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

type SessionInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, SessionInfo{
			ID:        s.ID,
			Name:      s.GetName(),
			CreatedAt: s.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt < list[j].CreatedAt
	})
	return list
}

func (m *Manager) Rename(id, name string) error {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("session %s not found", id)
	}
	s.SetName(name)
	return nil
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()

	s.Close()
	return nil
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.Close()
		delete(m.sessions, id)
	}
}

func (m *Manager) DataDir() string {
	return m.dataDir
}
