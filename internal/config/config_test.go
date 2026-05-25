package config

import (
	"strings"
	"testing"
)

func TestNormalizeRequiresBackend(t *testing.T) {
	cfg := Config{}
	err := cfg.Normalize()
	if err == nil {
		t.Fatalf("normalize succeeded, want error")
	}
	if !strings.Contains(err.Error(), "at least one backend is required") {
		t.Fatalf("error = %q, want backend requirement", err)
	}
}

func TestNormalizeRequiresClaudeCommand(t *testing.T) {
	cfg := Config{
		DefaultBackend: DefaultClaudeBackendKey,
		Backends: []BackendConfig{{
			Key: DefaultClaudeBackendKey,
		}},
	}
	err := cfg.Normalize()
	if err == nil {
		t.Fatalf("normalize succeeded, want error")
	}
	if !strings.Contains(err.Error(), "backends[0].command is required") {
		t.Fatalf("error = %q, want command requirement", err)
	}
}

func TestNormalizeKeepsExplicitClaudeBackend(t *testing.T) {
	cfg := Config{
		DefaultBackend: DefaultClaudeBackendKey,
		Backends: []BackendConfig{{
			Key:     DefaultClaudeBackendKey,
			Command: "claude-cli-acp",
			Args:    []string{"-claude", "claude"},
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	backend := cfg.Backends[0]
	if backend.Command != "claude-cli-acp" {
		t.Fatalf("command = %q, want explicit command", backend.Command)
	}
	if got, want := len(backend.Args), 2; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}
	if !backend.Capabilities.ReadTextFile() || !backend.Capabilities.WriteTextFile() || !backend.Capabilities.TerminalEnabled() {
		t.Fatalf("claude capabilities were not enabled: %#v", backend.Capabilities)
	}
}

func TestNormalizeNonClaudeDefaultsAreRestricted(t *testing.T) {
	cfg := Config{
		DefaultBackend: "fake",
		Backends: []BackendConfig{{
			Key:     "fake",
			Command: "fake-acp",
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	caps := cfg.Backends[0].Capabilities
	if !caps.ReadTextFile() {
		t.Fatalf("read capability should default to enabled")
	}
	if caps.WriteTextFile() || caps.TerminalEnabled() {
		t.Fatalf("non-claude backend should not default to write/terminal: %#v", caps)
	}
}
