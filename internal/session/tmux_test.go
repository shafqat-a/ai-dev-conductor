package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shafqat-a/ai-dev-conductor/internal/store"
)

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

// waitForSnapshot polls a session's snapshot until it contains want, or fails.
func waitForSnapshot(t *testing.T, s *Session, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(string(s.Snapshot()), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; snapshot was:\n%s", want, s.Snapshot())
}

// TestTmuxSessionSurvivesRestart is the M2a acceptance test: a session created in
// one "process" is still listed, attachable, and carries its pre-restart
// scrollback after a simulated conductor restart (CloseAll → fresh Manager).
func TestTmuxSessionSurvivesRestart(t *testing.T) {
	requireTmux(t)
	shell := testShell(t)
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// First process: create a tmux-backed session and produce some output.
	m1 := NewManager(shell, dir, st, 0, 0, true)
	s1, err := m1.Create("survivor")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := s1.ID

	// Guarantee the tmux server/session is gone even if the test bails early.
	t.Cleanup(func() {
		_ = tmuxKillSession(dir, tmuxName(id))
		_ = exec.Command("tmux", "-S", tmuxSocket(dir), "kill-server").Run()
	})

	if !s1.tmux {
		t.Fatal("expected a tmux-backed session when useTmux=true")
	}
	if err := s1.WriteInput([]byte("echo SENTINEL_BEFORE\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitForSnapshot(t, s1, "SENTINEL_BEFORE", 5*time.Second)

	// Simulate a restart: detach everything; the tmux session must outlive it.
	m1.CloseAll()
	if !tmuxHasSession(dir, tmuxName(id)) {
		t.Fatal("tmux session did not survive CloseAll (was killed, not detached)")
	}

	// Second process: a fresh Manager over the same dir/store reattaches on boot.
	m2 := NewManager(shell, dir, st, 0, 0, true)
	t.Cleanup(m2.CloseAll)

	s2, ok := m2.Get(id)
	if !ok {
		t.Fatalf("session %s was not reattached after restart", id)
	}
	if s2 == s1 {
		t.Fatal("expected a fresh Session instance after reattach")
	}

	// Pre-restart scrollback is intact, and the reattached PTY still drives the
	// same live shell.
	waitForSnapshot(t, s2, "SENTINEL_BEFORE", 5*time.Second)
	if err := s2.WriteInput([]byte("echo SENTINEL_AFTER\n")); err != nil {
		t.Fatalf("write after reattach: %v", err)
	}
	waitForSnapshot(t, s2, "SENTINEL_AFTER", 5*time.Second)

	// It reports as running in the merged list.
	var found bool
	for _, info := range m2.List() {
		if info.ID == id {
			found = true
			if info.Status != string(store.StatusRunning) {
				t.Errorf("status = %q, want running", info.Status)
			}
		}
	}
	if !found {
		t.Errorf("session %s missing from List() after reattach", id)
	}

	// Delete must tear down the underlying tmux session, not just detach.
	if err := m2.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if tmuxHasSession(dir, tmuxName(id)) {
		t.Error("tmux session still alive after Delete")
	}
}

// TestTmuxListSessionsFiltersPrefix verifies our session enumeration ignores
// tmux sessions that aren't ours (no aidc_ prefix).
func TestTmuxListSessionsFiltersPrefix(t *testing.T) {
	requireTmux(t)
	dir := t.TempDir()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", tmuxSocket(dir), "kill-server").Run()
	})

	// A foreign session on the same socket must not be reported.
	if err := exec.Command("tmux", "-S", tmuxSocket(dir), "new-session", "-d", "-s", "someone-elses").Run(); err != nil {
		t.Fatalf("seed foreign session: %v", err)
	}
	// One of ours.
	if err := exec.Command("tmux", "-S", tmuxSocket(dir), "new-session", "-d", "-s", tmuxName("abc123")).Run(); err != nil {
		t.Fatalf("seed our session: %v", err)
	}

	got := tmuxListSessions(dir)
	if len(got) != 1 || got[0] != "abc123" {
		t.Errorf("tmuxListSessions = %v, want [abc123]", got)
	}
}
