package session

import (
	"testing"
	"time"
)

func TestReapsIdleUnattachedSession(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(testShell(t), t.TempDir(), st, 50*time.Millisecond, 0, false)
	t.Cleanup(m.CloseAll)

	s, err := m.Create("idle")
	if err != nil {
		t.Fatal(err)
	}
	// Never attached → idle from creation. Wait past the timeout and reap.
	time.Sleep(120 * time.Millisecond)
	m.reapOnce()

	if _, ok := m.Get(s.ID); ok {
		t.Fatal("idle session should have been reaped")
	}
	if rows, _ := st.List(); len(rows) != 0 {
		t.Fatalf("reaped session should be gone from store, got %d rows", len(rows))
	}
}

func TestDoesNotReapAttachedSession(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(testShell(t), t.TempDir(), st, 50*time.Millisecond, 0, false)
	t.Cleanup(m.CloseAll)

	s, _ := m.Create("active")
	s.AddClient() // attached → never idle
	time.Sleep(120 * time.Millisecond)
	m.reapOnce()

	if _, ok := m.Get(s.ID); !ok {
		t.Fatal("attached session must not be reaped")
	}
}

func TestReapsAfterClientLeaves(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(testShell(t), t.TempDir(), st, 50*time.Millisecond, 0, false)
	t.Cleanup(m.CloseAll)

	s, _ := m.Create("x")
	c := s.AddClient()
	m.reapOnce() // attached: survives
	if _, ok := m.Get(s.ID); !ok {
		t.Fatal("should survive while attached")
	}

	s.RemoveClient(c)
	time.Sleep(120 * time.Millisecond)
	m.reapOnce()
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("should be reaped after client left and went idle")
	}
}

func TestMaxSessionsLimit(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(testShell(t), t.TempDir(), st, 0, 2, false)
	t.Cleanup(m.CloseAll)

	if _, err := m.Create("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("c"); err != ErrSessionLimit {
		t.Fatalf("want ErrSessionLimit on 3rd session, got %v", err)
	}
	// Deleting one frees a slot.
	list := m.List()
	if err := m.Delete(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("d"); err != nil {
		t.Fatalf("should allow create after delete: %v", err)
	}
}
