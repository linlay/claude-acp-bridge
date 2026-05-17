package acpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"proxy-acp/internal/config"
	"proxy-acp/internal/platform"
)

type EventSink interface {
	Publish(platform.EventData) error
}

type Manager struct {
	cfg config.Config

	mu       sync.Mutex
	sessions map[string]*backendSession
	awaiting map[string]*pendingPermission
	runs     map[string]*backendSession
}

func NewManager(cfg config.Config) *Manager {
	return &Manager{
		cfg:      cfg,
		sessions: map[string]*backendSession{},
		awaiting: map[string]*pendingPermission{},
		runs:     map[string]*backendSession{},
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*backendSession, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.sessions = map[string]*backendSession{}
	m.runs = map[string]*backendSession{}
	m.awaiting = map[string]*pendingPermission{}
	m.mu.Unlock()

	for _, sess := range sessions {
		sess.Close()
	}
}

func (m *Manager) Execute(ctx context.Context, req platform.QueryRequest, sink EventSink) error {
	backend, cwd, err := m.resolveBackend(req)
	if err != nil {
		return err
	}
	sess, err := m.session(ctx, backend, req.ChatID, cwd)
	if err != nil {
		return err
	}

	turn := newTurn(req, sink, m)
	m.registerRun(req.RunID, sess)
	defer m.unregisterRun(req.RunID)

	return sess.prompt(ctx, req, turn)
}

func (m *Manager) Submit(req platform.SubmitRequest) (platform.SubmitResponse, bool) {
	m.mu.Lock()
	pending := m.awaiting[req.AwaitingID]
	m.mu.Unlock()
	if pending == nil || pending.runID != req.RunID {
		return platform.SubmitResponse{
			Accepted:   false,
			Status:     "unmatched",
			RunID:      req.RunID,
			AwaitingID: req.AwaitingID,
			Detail:     "No active ACP permission request matched this submit",
		}, false
	}

	outcome := permissionOutcomeFromSubmit(req.Params, pending.options)
	select {
	case pending.response <- outcome:
		return platform.SubmitResponse{
			Accepted:   true,
			Status:     "accepted",
			RunID:      req.RunID,
			AwaitingID: req.AwaitingID,
			Detail:     "ACP permission response forwarded",
		}, true
	case <-pending.done:
		return platform.SubmitResponse{
			Accepted:   false,
			Status:     "stale",
			RunID:      req.RunID,
			AwaitingID: req.AwaitingID,
			Detail:     "ACP permission request already completed",
		}, true
	case <-time.After(2 * time.Second):
		return platform.SubmitResponse{
			Accepted:   false,
			Status:     "timeout",
			RunID:      req.RunID,
			AwaitingID: req.AwaitingID,
			Detail:     "Timed out forwarding ACP permission response",
		}, true
	}
}

func (m *Manager) Interrupt(req platform.InterruptRequest) platform.InterruptResponse {
	m.mu.Lock()
	sess := m.runs[req.RunID]
	m.mu.Unlock()
	if sess == nil {
		return platform.InterruptResponse{Accepted: false, Status: "unmatched", RunID: req.RunID, Detail: "No active ACP run matched interrupt"}
	}
	if err := sess.cancel(context.Background()); err != nil {
		return platform.InterruptResponse{Accepted: false, Status: "error", RunID: req.RunID, Detail: err.Error()}
	}
	return platform.InterruptResponse{Accepted: true, Status: "accepted", RunID: req.RunID, Detail: "ACP session cancel forwarded"}
}

func (m *Manager) registerAwaiting(p *pendingPermission) {
	m.mu.Lock()
	m.awaiting[p.awaitingID] = p
	m.mu.Unlock()
}

func (m *Manager) unregisterAwaiting(awaitingID string, p *pendingPermission) {
	m.mu.Lock()
	if m.awaiting[awaitingID] == p {
		delete(m.awaiting, awaitingID)
	}
	m.mu.Unlock()
}

func (m *Manager) registerRun(runID string, sess *backendSession) {
	m.mu.Lock()
	m.runs[runID] = sess
	m.mu.Unlock()
}

func (m *Manager) unregisterRun(runID string) {
	m.mu.Lock()
	delete(m.runs, runID)
	m.mu.Unlock()
}

func (m *Manager) resolveBackend(req platform.QueryRequest) (config.BackendConfig, string, error) {
	key := stringParam(req.Params, "backend")
	if key == "" {
		key = strings.TrimSpace(req.AgentKey)
	}
	backend, ok := m.cfg.Backend(key)
	if !ok {
		backend, ok = m.cfg.Backend("")
	}
	if !ok {
		return config.BackendConfig{}, "", fmt.Errorf("backend %q not configured", key)
	}
	cwd := stringParam(req.Params, "cwd")
	if cwd == "" {
		cwd = backend.DefaultCwd
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return config.BackendConfig{}, "", err
	}
	return backend, abs, nil
}

func (m *Manager) session(ctx context.Context, backend config.BackendConfig, chatID string, cwd string) (*backendSession, error) {
	if strings.TrimSpace(chatID) == "" {
		chatID = "default"
	}
	key := backend.Key + "\x00" + chatID + "\x00" + cwd

	m.mu.Lock()
	if existing := m.sessions[key]; existing != nil && existing.alive() {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	sess, err := startBackendSession(ctx, backend, chatID, cwd)
	if err != nil {
		return nil, err
	}
	sess.onExit = func() {
		m.mu.Lock()
		if m.sessions[key] == sess {
			delete(m.sessions, key)
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	if existing := m.sessions[key]; existing != nil && existing.alive() {
		m.mu.Unlock()
		sess.Close()
		return existing, nil
	}
	m.sessions[key] = sess
	m.mu.Unlock()
	return sess, nil
}

type backendSession struct {
	cfg       config.BackendConfig
	chatID    string
	cwd       string
	sessionID acp.SessionId
	cmd       *exec.Cmd
	conn      *acp.ClientSideConnection
	client    *bridgeClient
	onExit    func()

	promptMu sync.Mutex
	mu       sync.Mutex
	active   *turn
	closed   bool
}

func startBackendSession(ctx context.Context, cfg config.BackendConfig, chatID string, cwd string) (*backendSession, error) {
	cmd := exec.CommandContext(context.Background(), cfg.Command, cfg.Args...)
	cmd.Dir = cwd
	cmd.Env = mergeEnv(os.Environ(), cfg.Env)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	sess := &backendSession{cfg: cfg, chatID: chatID, cwd: cwd, cmd: cmd}
	client := &bridgeClient{session: sess}
	conn := acp.NewClientSideConnection(client, stdin, stdout)
	conn.SetLogger(slog.Default())
	sess.client = client
	sess.conn = conn

	go func() {
		err := cmd.Wait()
		if err != nil && !sess.isClosed() {
			log.Printf("[proxy-acp][backend:%s] exited: %v", cfg.Key, err)
		}
		if sess.onExit != nil {
			sess.onExit()
		}
	}()

	initCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "proxy-acp", Version: "0.1.0"},
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: false},
			Terminal: false,
		},
	}); err != nil {
		sess.Close()
		return nil, fmt.Errorf("initialize ACP backend %q: %w", cfg.Key, err)
	}
	newSession, err := conn.NewSession(initCtx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("create ACP session for backend %q: %w", cfg.Key, err)
	}
	sess.sessionID = newSession.SessionId
	return sess, nil
}

func (s *backendSession) prompt(ctx context.Context, req platform.QueryRequest, t *turn) error {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("backend session is closed")
	}
	s.active = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active == t {
			s.active = nil
		}
		s.mu.Unlock()
	}()

	t.emit("request.query", map[string]any{
		"requestId":  req.RequestID,
		"runId":      req.RunID,
		"chatId":     req.ChatID,
		"agentKey":   req.AgentKey,
		"role":       req.Role,
		"message":    req.Message,
		"references": req.References,
		"params":     req.Params,
		"scene":      req.Scene,
	})
	t.emit("run.start", map[string]any{"runId": req.RunID, "chatId": req.ChatID, "agentKey": req.AgentKey})

	prompt, err := promptBlocks(req)
	if err != nil {
		t.emitRunError(err)
		return err
	}

	resp, err := s.conn.Prompt(ctx, acp.PromptRequest{SessionId: s.sessionID, Prompt: prompt})
	t.closeOpenStreams()
	if err != nil {
		t.emitRunError(err)
		return err
	}

	switch resp.StopReason {
	case acp.StopReasonCancelled:
		t.emit("run.cancel", map[string]any{"runId": req.RunID, "usage": usagePayload(resp.Usage)})
	default:
		t.emit("run.complete", map[string]any{"runId": req.RunID, "finishReason": string(resp.StopReason), "usage": usagePayload(resp.Usage)})
	}
	return nil
}

func (s *backendSession) cancel(ctx context.Context) error {
	s.mu.Lock()
	sessionID := s.sessionID
	s.mu.Unlock()
	if sessionID == "" {
		return fmt.Errorf("ACP session is not initialized")
	}
	return s.conn.Cancel(ctx, acp.CancelNotification{SessionId: sessionID})
}

func (s *backendSession) activeTurn() *turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *backendSession) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	select {
	case <-s.conn.Done():
		return false
	default:
		return true
	}
}

func (s *backendSession) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (s *backendSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type bridgeClient struct {
	session *backendSession
}

func (c *bridgeClient) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	t := c.session.activeTurn()
	if t == nil {
		return nil
	}
	return t.handleUpdate(params.Update)
}

func (c *bridgeClient) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	t := c.session.activeTurn()
	if t == nil {
		return acp.RequestPermissionResponse{}, fmt.Errorf("permission request without active turn")
	}
	return t.requestPermission(ctx, params)
}

func (c *bridgeClient) ReadTextFile(_ context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	data, err := os.ReadFile(params.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	text := string(data)
	if params.Line != nil || params.Limit != nil {
		text = sliceTextLines(text, params.Line, params.Limit)
	}
	return acp.ReadTextFileResponse{Content: text}, nil
}

func (c *bridgeClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("proxy-acp does not allow ACP backends to write files directly")
}

func (c *bridgeClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("proxy-acp does not expose terminal execution in v1")
}

func (c *bridgeClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("proxy-acp does not expose terminal execution in v1")
}

func (c *bridgeClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("proxy-acp does not expose terminal execution in v1")
}

func (c *bridgeClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("proxy-acp does not expose terminal execution in v1")
}

func (c *bridgeClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("proxy-acp does not expose terminal execution in v1")
}

func promptBlocks(req platform.QueryRequest) ([]acp.ContentBlock, error) {
	blocks := []acp.ContentBlock{acp.TextBlock(req.Message)}
	for _, ref := range req.References {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			name = strings.TrimSpace(ref.ID)
		}
		if name == "" {
			name = "reference"
		}
		uri := strings.TrimSpace(ref.URL)
		if uri == "" && ref.SandboxPath != "" {
			abs, err := filepath.Abs(ref.SandboxPath)
			if err != nil {
				return nil, err
			}
			uri = (&url.URL{Scheme: "file", Path: abs}).String()
		}
		if uri == "" {
			continue
		}
		block := acp.ResourceLinkBlock(name, uri)
		if ref.MimeType != "" {
			block.ResourceLink.MimeType = acp.Ptr(ref.MimeType)
		}
		if ref.SizeBytes != nil {
			size := int(*ref.SizeBytes)
			block.ResourceLink.Size = &size
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func mergeEnv(base []string, overrides map[string]string) []string {
	out := append([]string(nil), base...)
	for key, value := range overrides {
		prefix := key + "="
		replaced := false
		for idx := range out {
			if strings.HasPrefix(out[idx], prefix) {
				out[idx] = prefix + value
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, prefix+value)
		}
	}
	return out
}

func stringParam(params map[string]any, key string) string {
	if params == nil {
		return ""
	}
	value, ok := params[key]
	if !ok {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func usagePayload(usage *acp.Usage) map[string]any {
	if usage == nil {
		return nil
	}
	out := map[string]any{
		"promptTokens":     usage.InputTokens,
		"completionTokens": usage.OutputTokens,
		"totalTokens":      usage.TotalTokens,
	}
	if usage.ThoughtTokens != nil {
		out["reasoningTokens"] = *usage.ThoughtTokens
	}
	if usage.CachedReadTokens != nil {
		out["cachedReadTokens"] = *usage.CachedReadTokens
	}
	if usage.CachedWriteTokens != nil {
		out["cachedWriteTokens"] = *usage.CachedWriteTokens
	}
	return out
}

func sliceTextLines(content string, line *int, limit *int) string {
	lines := strings.Split(content, "\n")
	start := 0
	if line != nil && *line > 0 {
		start = *line - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit != nil && *limit >= 0 && start+*limit < end {
		end = start + *limit
	}
	return strings.Join(lines[start:end], "\n")
}

func marshalAny(value any) string {
	if value == nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}
