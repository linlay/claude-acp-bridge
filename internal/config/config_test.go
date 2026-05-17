package config

import (
	"reflect"
	"testing"
)

func TestNormalizeAddsDefaultClaudeBackend(t *testing.T) {
	cfg := Config{}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.DefaultBackend != DefaultClaudeBackendKey {
		t.Fatalf("defaultBackend = %q, want %q", cfg.DefaultBackend, DefaultClaudeBackendKey)
	}
	backend, ok := cfg.Backend("")
	if !ok {
		t.Fatalf("default backend not found")
	}
	if backend.Command != DefaultClaudeCommand {
		t.Fatalf("command = %q, want %q", backend.Command, DefaultClaudeCommand)
	}
	if !reflect.DeepEqual(backend.Args, DefaultClaudeArgs()) {
		t.Fatalf("args = %#v, want %#v", backend.Args, DefaultClaudeArgs())
	}
	if !backend.Capabilities.ReadTextFile() || !backend.Capabilities.WriteTextFile() || !backend.Capabilities.TerminalEnabled() {
		t.Fatalf("claude capabilities were not enabled: %#v", backend.Capabilities)
	}
}

func TestNormalizeCompletesClaudeBackendCommand(t *testing.T) {
	cfg := Config{
		DefaultBackend: DefaultClaudeBackendKey,
		Backends: []BackendConfig{{
			Key: DefaultClaudeBackendKey,
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	backend := cfg.Backends[0]
	if backend.Command != DefaultClaudeCommand {
		t.Fatalf("command = %q, want %q", backend.Command, DefaultClaudeCommand)
	}
	if !reflect.DeepEqual(backend.Args, DefaultClaudeArgs()) {
		t.Fatalf("args = %#v, want %#v", backend.Args, DefaultClaudeArgs())
	}
}

func TestNormalizeKeepsExplicitClaudeCommand(t *testing.T) {
	cfg := Config{
		DefaultBackend: DefaultClaudeBackendKey,
		Backends: []BackendConfig{{
			Key:     DefaultClaudeBackendKey,
			Command: "claude-agent-acp",
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	backend := cfg.Backends[0]
	if backend.Command != "claude-agent-acp" {
		t.Fatalf("command = %q, want explicit command", backend.Command)
	}
	if len(backend.Args) != 0 {
		t.Fatalf("args = %#v, want explicit empty args", backend.Args)
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
