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
		PublicURL:        strings.TrimRight(os.Getenv("AI_CONDUCTOR_PUBLIC_URL"), "/"),
		ShareTTL:         envDuration("AI_CONDUCTOR_SHARE_TTL", 24*time.Hour),
	}

	if cfg.Shell == "" {
		cfg.Shell = detectShell()
	}

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
	return nil
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

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
