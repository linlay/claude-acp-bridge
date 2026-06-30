package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"proxy-acp/internal/claudecli"
	"proxy-acp/internal/config"
	"proxy-acp/internal/platform"
)

const (
	claudeModelDiscoveryPrompt  = "请只回复 OK。"
	claudeModelDiscoveryTimeout = 30 * time.Second
	claudeModelCatalogTTL       = 10 * time.Minute
)

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	catalog, err := s.queryClaudeModels(r.Context())
	if err != nil {
		platform.WriteJSON(w, http.StatusBadGateway, platform.Failure(http.StatusBadGateway, err.Error()))
		return
	}
	platform.WriteJSON(w, http.StatusOK, platform.Success(catalog))
}

func (s *Server) queryClaudeModels(ctx context.Context) (platform.ModelCatalogResponse, error) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if time.Now().Before(s.modelsCacheUntil) && len(s.modelsCache.Models) > 0 {
		return s.modelsCache, nil
	}

	catalog, err := s.discoverClaudeModels(ctx)
	if err != nil {
		return platform.ModelCatalogResponse{}, err
	}
	s.modelsCache = catalog
	s.modelsCacheUntil = time.Now().Add(claudeModelCatalogTTL)
	return catalog, nil
}

func (s *Server) discoverClaudeModels(ctx context.Context) (platform.ModelCatalogResponse, error) {
	command, baseArgs, env, err := claudeModelProbeCommand(s.cfg)
	if err != nil {
		return platform.ModelCatalogResponse{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, claudeModelDiscoveryTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, command, claudeModelProbeArgs(baseArgs)...)
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	cmd.Env = append(os.Environ(), env...)
	output, runErr := cmd.CombinedOutput()
	if runCtx.Err() != nil {
		return platform.ModelCatalogResponse{}, fmt.Errorf("query claude models timed out after %s", claudeModelDiscoveryTimeout)
	}

	catalog, parseErr := parseClaudeModelCatalog(output)
	if parseErr == nil {
		return catalog, nil
	}
	if runErr != nil {
		return platform.ModelCatalogResponse{}, fmt.Errorf("query claude models: %w: %s", runErr, compactCommandOutput(output))
	}
	return platform.ModelCatalogResponse{}, parseErr
}

func claudeModelProbeCommand(cfg config.Config) (string, []string, []string, error) {
	backend, ok := cfg.Backend(cfg.DefaultBackend)
	if !ok {
		return "", nil, nil, fmt.Errorf("default backend %q is not configured", cfg.DefaultBackend)
	}
	if backend.Command != config.SelfBackendCommand {
		return "", nil, nil, fmt.Errorf("backend %q does not use the internal Claude ACP backend", backend.Key)
	}

	command := "claude"
	args := []string{}
	runtimeOptions := claudecli.RuntimeOptions{}
	for idx := 0; idx < len(backend.Args); idx++ {
		arg := strings.TrimSpace(backend.Args[idx])
		switch arg {
		case "", config.ClaudeBackendModeArg:
			continue
		case "-claude":
			value, next, err := nextBackendArg(backend.Args, idx, "-claude")
			if err != nil {
				return "", nil, nil, err
			}
			command = value
			idx = next
		case "-arg":
			value, next, err := nextBackendArg(backend.Args, idx, "-arg")
			if err != nil {
				return "", nil, nil, err
			}
			args = append(args, value)
			idx = next
		case "-model":
			value, next, err := nextBackendArg(backend.Args, idx, "-model")
			if err != nil {
				return "", nil, nil, err
			}
			runtimeOptions.Model = value
			idx = next
		case "-effort":
			value, next, err := nextBackendArg(backend.Args, idx, "-effort")
			if err != nil {
				return "", nil, nil, err
			}
			runtimeOptions.Effort = value
			idx = next
		case "-permission-mode":
			value, next, err := nextBackendArg(backend.Args, idx, "-permission-mode")
			if err != nil {
				return "", nil, nil, err
			}
			runtimeOptions.PermissionMode = value
			idx = next
		case "-dangerously-skip-permissions":
			runtimeOptions.DangerouslySkipPermissions = true
		}
	}
	args = claudecli.ApplyRuntimeArgs(args, runtimeOptions)

	var env []string
	for key, value := range backend.Env {
		key = strings.TrimSpace(key)
		if key != "" {
			env = append(env, key+"="+value)
		}
	}
	sort.Strings(env)
	return command, args, env, nil
}

func nextBackendArg(args []string, idx int, flag string) (string, int, error) {
	if idx+1 >= len(args) || strings.TrimSpace(args[idx+1]) == "" {
		return "", idx, fmt.Errorf("%s requires a value", flag)
	}
	return args[idx+1], idx + 1, nil
}

func claudeModelProbeArgs(base []string) []string {
	args := append([]string(nil), base...)
	args = append(args, "-p", "--verbose", "--output-format", "stream-json")
	if !hasClaudeArg(args, "--include-partial-messages") {
		args = append(args, "--include-partial-messages")
	}
	if !hasClaudeArg(args, "--no-session-persistence") {
		args = append(args, "--no-session-persistence")
	}
	return append(args, claudeModelDiscoveryPrompt)
}

func hasClaudeArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func compactCommandOutput(output []byte) string {
	value := strings.TrimSpace(string(output))
	if value == "" {
		return ""
	}
	const limit = 600
	if len(value) > limit {
		return value[:limit] + "..."
	}
	return value
}
