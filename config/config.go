package config

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

type Config struct {
	Password       string
	ListenAddr     string
	DataDir        string
	Shell          string
	SessionTimeout time.Duration
	PIDFile        string
}

func Load() (*Config, error) {
	cfg := &Config{
		Password:       envOrDefault("AI_CONDUCTOR_PASSWORD", "admin"),
		ListenAddr:     envOrDefault("AI_CONDUCTOR_ADDR", "0.0.0.0:8080"),
		DataDir:        envOrDefault("AI_CONDUCTOR_DATA_DIR", "./data/sessions"),
		Shell:          envOrDefault("AI_CONDUCTOR_SHELL", ""),
		SessionTimeout: 24 * time.Hour,
		PIDFile:        os.Getenv("AI_CONDUCTOR_PID_FILE"),
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
