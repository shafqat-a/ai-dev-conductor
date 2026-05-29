package config

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Password       string
	ListenAddr     string
	DataDir        string
	Shell          string
	SessionTimeout time.Duration
	PIDFile        string

	// BasePath is the URL path prefix the app is served under (e.g. "/terminaltest"
	// when behind a reverse proxy at example.com/terminaltest). Empty means the app
	// owns the host root. Normalized to a leading slash and no trailing slash.
	BasePath string

	// PublicURL is the externally-reachable base URL (e.g. https://host) used to
	// build absolute share-link URLs. Empty falls back to the request's origin.
	PublicURL string
	// ShareTTL is the default lifetime of a freshly-minted share link.
	ShareTTL time.Duration

	// Login brute-force protection. LoginMaxAttempts <= 0 disables it.
	LoginMaxAttempts int
	LoginWindow      time.Duration
	LoginLockout     time.Duration

	// Session lifecycle. IdleTimeout <= 0 disables reaping; MaxSessions <= 0 is unlimited.
	IdleTimeout time.Duration
	MaxSessions int

	// MaxUploadBytes caps the size of a single file uploaded to a session's working
	// directory. Keep in sync with nginx client_max_body_size when behind a proxy.
	MaxUploadBytes int64

	// TmuxBin is the resolved path to the tmux executable. tmux is a hard
	// requirement: every session runs inside a detached tmux session so it
	// survives a conductor restart, and the server refuses to start without it.
	TmuxBin string
}

func Load() (*Config, error) {
	cfg := &Config{
		Password:         envOrDefault("AI_CONDUCTOR_PASSWORD", "admin"),
		ListenAddr:       envOrDefault("AI_CONDUCTOR_ADDR", "0.0.0.0:8080"),
		DataDir:          envOrDefault("AI_CONDUCTOR_DATA_DIR", "./data/sessions"),
		Shell:            envOrDefault("AI_CONDUCTOR_SHELL", ""),
		SessionTimeout:   24 * time.Hour,
		PIDFile:          os.Getenv("AI_CONDUCTOR_PID_FILE"),
		LoginMaxAttempts: envInt("AI_CONDUCTOR_LOGIN_MAX_ATTEMPTS", 5),
		LoginWindow:      envDuration("AI_CONDUCTOR_LOGIN_WINDOW", time.Minute),
		LoginLockout:     envDuration("AI_CONDUCTOR_LOGIN_LOCKOUT", time.Minute),
		IdleTimeout:      envDuration("AI_CONDUCTOR_IDLE_TIMEOUT", 0),
		MaxSessions:      envInt("AI_CONDUCTOR_MAX_SESSIONS", 0),
		MaxUploadBytes:   envInt64("AI_CONDUCTOR_MAX_UPLOAD_BYTES", 100<<20), // 100 MiB
		PublicURL:        strings.TrimRight(os.Getenv("AI_CONDUCTOR_PUBLIC_URL"), "/"),
		ShareTTL:         envDuration("AI_CONDUCTOR_SHARE_TTL", 24*time.Hour),
		BasePath:         normalizeBasePath(os.Getenv("AI_CONDUCTOR_BASE_PATH")),
	}

	if cfg.Shell == "" {
		cfg.Shell = detectShell()
	}

	// tmux is mandatory; resolve its path now so Validate can enforce presence.
	cfg.TmuxBin, _ = exec.LookPath("tmux")

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Password == "" {
		return fmt.Errorf("password must not be empty")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen address must not be empty")
	}
	if c.Shell == "" {
		return fmt.Errorf("no shell found; set AI_CONDUCTOR_SHELL")
	}
	if _, err := exec.LookPath(c.Shell); err != nil {
		return fmt.Errorf("shell %q not found: %w", c.Shell, err)
	}
	if c.TmuxBin == "" {
		return fmt.Errorf("tmux is required but was not found in PATH; install tmux")
	}
	return nil
}

// normalizeBasePath cleans a configured base path into "" (root) or "/prefix"
// with a leading slash and no trailing slash. Nested prefixes are preserved.
func normalizeBasePath(raw string) string {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

func detectShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	for _, sh := range []string{"bash", "zsh", "sh"} {
		if path, err := exec.LookPath(sh); err == nil {
			return path
		}
	}
	return "/bin/sh"
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
