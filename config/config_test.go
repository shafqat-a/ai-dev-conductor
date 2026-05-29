package config

import (
	"strings"
	"testing"
)

func baseValidConfig() *Config {
	return &Config{
		Password:   "secret",
		ListenAddr: "0.0.0.0:8080",
		Shell:      "/bin/sh",
		TmuxBin:    "/usr/bin/tmux",
	}
}

func TestValidateRequiresTmux(t *testing.T) {
	c := baseValidConfig()
	c.TmuxBin = "" // simulate tmux missing from PATH
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error when tmux is absent")
	}
	if !strings.Contains(err.Error(), "tmux") {
		t.Errorf("error %q does not mention tmux", err)
	}
}

func TestValidatePassesWithTmux(t *testing.T) {
	if err := baseValidConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestNormalizeBasePath(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"/":                 "",
		"  ":                "",
		"terminaltest":      "/terminaltest",
		"/terminaltest":     "/terminaltest",
		"/terminaltest/":    "/terminaltest",
		"  /terminaltest/ ": "/terminaltest",
		"a/b":               "/a/b",
		"/a/b/":             "/a/b",
	}
	for in, want := range cases {
		if got := normalizeBasePath(in); got != want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}
