package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shafqat-a/ai-dev-conductor/internal/store"
)

func testShell(t *testing.T) string {
	t.Helper()
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	t.Skip("no shell available")
	return ""
}

// newTestManager returns a Manager backed by a temp store, cleaned up on exit.
func newTestManager(t *testing.T, st *store.Store) *Manager {
	t.Helper()
	m := NewManager(testShell(t), t.TempDir(), st, 0, 0)
	t.Cleanup(m.CloseAll)
	return m
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestManagerCreatePersists(t *testing.T) {
	st := newTestStore(t)
	m := newTestManager(t, st)

	if _, err := m.Create("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("two"); err != nil {
		t.Fatal(err)
	}

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}
	for _, s := range list {
		if s.Status != string(store.StatusRunning) {
			t.Fatalf("live session should be running, got %q", s.Status)
		}
	}
	if rows, _ := st.List(); len(rows) != 2 {
		t.Fatalf("store should have 2 rows, got %d", len(rows))
	}
}

func TestManagerRenamePropagatesToStore(t *testing.T) {
	st := newTestStore(t)
	m := newTestManager(t, st)
	s, _ := m.Create("orig")

	if err := m.Rename(s.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	if got := s.GetName(); got != "renamed" {
		t.Fatalf("live name not updated: %q", got)
	}
	rows, _ := st.List()
	if rows[0].Name != "renamed" {
		t.Fatalf("store name not updated: %q", rows[0].Name)
	}
}

func TestManagerDeleteRemovesFromStore(t *testing.T) {
	st := newTestStore(t)
	m := newTestManager(t, st)
	s, _ := m.Create("x")

	if err := m.Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("session should be gone from live map")
	}
	if rows, _ := st.List(); len(rows) != 0 {
		t.Fatalf("store should be empty, got %d rows", len(rows))
	}
}

func TestManagerReconcileOnRestart(t *testing.T) {
	st := newTestStore(t)
	dir := t.TempDir()

	m1 := NewManager(testShell(t), dir, st, 0, 0)
	m1.Create("a")
	m1.Create("b")
	m1.CloseAll() // simulates the process going away

	// A fresh Manager on the same store: no live PTYs, rows shown detached.
	m2 := NewManager(testShell(t), dir, st, 0, 0)
	t.Cleanup(m2.CloseAll)

	list := m2.List()
	if len(list) != 2 {
		t.Fatalf("want 2 persisted sessions after restart, got %d", len(list))
	}
	for _, s := range list {
		if s.Status != string(store.StatusDetached) {
			t.Fatalf("restarted session should be detached, got %q", s.Status)
		}
		if _, ok := m2.Get(s.ID); ok {
			t.Fatal("detached session must not be live")
		}
	}

	// Deleting a detached (non-live) session must still work.
	if err := m2.Delete(list[0].ID); err != nil {
		t.Fatalf("delete detached session: %v", err)
	}
	if rows, _ := st.List(); len(rows) != 1 {
		t.Fatalf("want 1 row after deleting detached, got %d", len(rows))
	}
}

func TestManagerReconcileFlipsRunningToDetached(t *testing.T) {
	st := newTestStore(t)
	// Seed a stale "running" row as if left by a crashed process.
	st.Upsert(store.SessionMeta{
		ID: "stale", Name: "stale", CreatedAt: 1, LastActivityAt: 1,
		Status: store.StatusRunning,
	})

	m := newTestManager(t, st) // NewManager calls MarkAllDetached
	list := m.List()
	if len(list) != 1 || list[0].Status != string(store.StatusDetached) {
		t.Fatalf("stale running row should be reconciled to detached: %+v", list)
	}
}

func TestManagerActivityFlush(t *testing.T) {
	st := newTestStore(t)
	m := newTestManager(t, st)
	s, _ := m.Create("x")

	s.WriteInput([]byte("echo hello\n"))
	time.Sleep(200 * time.Millisecond) // let the shell echo back

	m.flush()

	rows, _ := st.List()
	if rows[0].LastActivityAt != s.LastActivity() {
		t.Fatalf("flushed activity %d != session activity %d",
			rows[0].LastActivityAt, s.LastActivity())
	}
}

func TestManagerClientDisconnectTracking(t *testing.T) {
	st := newTestStore(t)
	m := newTestManager(t, st)
	s, _ := m.Create("x")

	c := s.AddClient()
	if disconnectAt(t, st, s.ID) != 0 {
		t.Fatal("attached session should have disconnect=0")
	}

	s.RemoveClient(c)
	if disconnectAt(t, st, s.ID) == 0 {
		t.Fatal("last client leaving should set a disconnect timestamp")
	}

	s.AddClient() // reattach clears it
	if disconnectAt(t, st, s.ID) != 0 {
		t.Fatal("reattaching should reset disconnect to 0")
	}
}

func disconnectAt(t *testing.T, st *store.Store, id string) int64 {
	t.Helper()
	rows, _ := st.List()
	for _, r := range rows {
		if r.ID == id {
			return r.LastClientDisconnectAt
		}
	}
	t.Fatalf("row %q not found", id)
	return 0
}
