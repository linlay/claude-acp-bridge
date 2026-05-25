package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultListenAddr    = "127.0.0.1:3210"
	defaultIdleTimeoutMs = int64(30 * time.Minute / time.Millisecond)

	DefaultClaudeBackendKey = "claude-code"
)

type Config struct {
	ListenAddr     string          `yaml:"listenAddr"`
	AuthToken      string          `yaml:"authToken"`
	DefaultBackend string          `yaml:"defaultBackend"`
	Backends       []BackendConfig `yaml:"backends"`
}

type BackendConfig struct {
	Key           string              `yaml:"key"`
	Command       string              `yaml:"command"`
	Args          []string            `yaml:"args"`
	Env           map[string]string   `yaml:"env"`
	DefaultCwd    string              `yaml:"defaultCwd"`
	IdleTimeoutMs int64               `yaml:"idleTimeoutMs"`
	Capabilities  BackendCapabilities `yaml:"capabilities"`
}

type BackendCapabilities struct {
	Fs       FileSystemCapabilities `yaml:"fs"`
	Terminal *bool                  `yaml:"terminal"`
}

type FileSystemCapabilities struct {
	ReadTextFile  *bool `yaml:"readTextFile"`
	WriteTextFile *bool `yaml:"writeTextFile"`
}

func Load(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("PROXY_ACP_CONFIG"))
	}
	if path == "" {
		if _, err := os.Stat("config.yml"); err == nil {
			path = "config.yml"
		} else if _, err := os.Stat("config.yaml"); err == nil {
			path = "config.yaml"
		}
	}

	cfg := Config{ListenAddr: defaultListenAddr}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.Normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Normalize() error {
	c.ListenAddr = strings.TrimSpace(c.ListenAddr)
	if c.ListenAddr == "" {
		c.ListenAddr = defaultListenAddr
	}
	if len(c.Backends) == 0 {
		return fmt.Errorf("at least one backend is required")
	}

	seen := map[string]struct{}{}
	for idx := range c.Backends {
		backend := &c.Backends[idx]
		backend.Key = strings.TrimSpace(backend.Key)
		backend.Command = strings.TrimSpace(backend.Command)
		backend.DefaultCwd = strings.TrimSpace(backend.DefaultCwd)
		if backend.Key == "" {
			return fmt.Errorf("backends[%d].key is required", idx)
		}
		if backend.Command == "" {
			return fmt.Errorf("backends[%d].command is required", idx)
		}
		if _, ok := seen[backend.Key]; ok {
			return fmt.Errorf("duplicate backend key %q", backend.Key)
		}
		seen[backend.Key] = struct{}{}
		if backend.DefaultCwd == "" {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			backend.DefaultCwd = wd
		}
		abs, err := filepath.Abs(backend.DefaultCwd)
		if err != nil {
			return fmt.Errorf("resolve defaultCwd for backend %q: %w", backend.Key, err)
		}
		backend.DefaultCwd = abs
		if backend.IdleTimeoutMs <= 0 {
			backend.IdleTimeoutMs = defaultIdleTimeoutMs
		}
		if backend.Env == nil {
			backend.Env = map[string]string{}
		}
		backend.Capabilities.normalize(backend.Key == DefaultClaudeBackendKey)
	}

	c.DefaultBackend = strings.TrimSpace(c.DefaultBackend)
	if c.DefaultBackend == "" && len(c.Backends) > 0 {
		c.DefaultBackend = c.Backends[0].Key
	}
	if _, ok := seen[c.DefaultBackend]; !ok {
		return fmt.Errorf("defaultBackend %q does not match any backend", c.DefaultBackend)
	}
	return nil
}

func (c Config) Backend(key string) (BackendConfig, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		key = c.DefaultBackend
	}
	for _, backend := range c.Backends {
		if backend.Key == key {
			return backend, true
		}
	}
	return BackendConfig{}, false
}

func (c *BackendCapabilities) normalize(isClaude bool) {
	if c.Fs.ReadTextFile == nil {
		c.Fs.ReadTextFile = boolPtr(true)
	}
	if c.Fs.WriteTextFile == nil {
		c.Fs.WriteTextFile = boolPtr(isClaude)
	}
	if c.Terminal == nil {
		c.Terminal = boolPtr(isClaude)
	}
}

func (c BackendCapabilities) ReadTextFile() bool {
	return c.Fs.ReadTextFile != nil && *c.Fs.ReadTextFile
}

func (c BackendCapabilities) WriteTextFile() bool {
	return c.Fs.WriteTextFile != nil && *c.Fs.WriteTextFile
}

func (c BackendCapabilities) TerminalEnabled() bool {
	return c.Terminal != nil && *c.Terminal
}

func boolPtr(v bool) *bool {
	return &v
}
