package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Sessions are backed by a private tmux server so shells survive a conductor
// restart. The server listens on a socket under the data dir (not /tmp) so it
// is unaffected by systemd PrivateTmp and isolated from the user's own tmux.
const (
	tmuxPrefix   = "aidc_" // namespaces our tmux sessions away from the user's
	captureLines = 2000    // scrollback lines seeded to a freshly-connected viewer
)

// tmuxName maps a conductor session ID to its tmux session name.
func tmuxName(id string) string { return tmuxPrefix + id }

// tmuxSocket is the path to our private tmux server socket.
func tmuxSocket(dataDir string) string { return filepath.Join(dataDir, "tmux.sock") }

// tmuxBaseArgs returns a fresh arg slice pinning our private server socket.
func tmuxBaseArgs(dataDir string) []string {
	return []string{"-S", tmuxSocket(dataDir)}
}

// tmuxEnv strips $TMUX so tmux never refuses to attach when the conductor itself
// happens to be running inside a tmux session (common in dev).
func tmuxEnv() []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TERM=xterm-256color")
}

// tmuxAttachCmd builds an attach-or-create command suitable for pty.Start. The
// `-A` flag makes it idempotent: it creates the session running shell the first
// time and performs a plain attach on every subsequent (post-restart) call.
func tmuxAttachCmd(dataDir, name, shell string) *exec.Cmd {
	args := append(tmuxBaseArgs(dataDir), "new-session", "-A", "-s", name, "--", shell)
	cmd := exec.Command("tmux", args...)
	cmd.Env = tmuxEnv()
	return cmd
}

// tmuxHasSession reports whether the named tmux session currently exists.
func tmuxHasSession(dataDir, name string) bool {
	args := append(tmuxBaseArgs(dataDir), "has-session", "-t", name)
	return exec.Command("tmux", args...).Run() == nil
}

// tmuxKillSession terminates the named tmux session (and its shell).
func tmuxKillSession(dataDir, name string) error {
	args := append(tmuxBaseArgs(dataDir), "kill-session", "-t", name)
	return exec.Command("tmux", args...).Run()
}

// tmuxListSessions returns the conductor session IDs (prefix stripped) of every
// live tmux session on our private socket. Returns nil when no server is running.
func tmuxListSessions(dataDir string) []string {
	args := append(tmuxBaseArgs(dataDir), "list-sessions", "-F", "#{session_name}")
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, tmuxPrefix) {
			ids = append(ids, strings.TrimPrefix(line, tmuxPrefix))
		}
	}
	return ids
}

// tmuxPanePID returns the PID of the shell running in the session's pane. Used to
// resolve the session's working directory via /proc/<pid>/cwd, since the conductor
// only holds the tmux client process, not the shell itself.
func tmuxPanePID(dataDir, name string) (int, error) {
	args := append(tmuxBaseArgs(dataDir), "display-message", "-p", "-t", name, "#{pane_pid}")
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// tmuxCapturePane returns the pane's recent scrollback (with SGR escapes) to seed
// a newly-connected viewer. tmux emits LF-separated lines; callers convert to CRLF.
func tmuxCapturePane(dataDir, name string, lines int) []byte {
	args := append(tmuxBaseArgs(dataDir),
		"capture-pane", "-t", name, "-e", "-p", "-S", "-"+strconv.Itoa(lines))
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return nil
	}
	return out
}
