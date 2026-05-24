package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, " ") }
func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

type agent struct {
	conn          *acp.AgentSideConnection
	claudeCommand string
	claudeArgs    []string

	mu       sync.Mutex
	sessions map[acp.SessionId]string
	active   map[acp.SessionId]*exec.Cmd
}

type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func main() {
	var extra multiFlag
	claudeCommand := flag.String("claude", "claude", "Claude Code CLI command")
	flag.Var(&extra, "arg", "Extra argument passed to Claude Code CLI, repeatable")
	flag.Parse()

	a := &agent{
		claudeCommand: *claudeCommand,
		claudeArgs:    extra,
		sessions:      map[acp.SessionId]string{},
		active:        map[acp.SessionId]*exec.Cmd{},
	}
	conn := acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	conn.SetLogger(slog.Default())
	a.conn = conn
	<-conn.Done()
}

func (a *agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (a *agent) NewSession(_ context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	id := acp.SessionId("sess_" + randomHex())
	a.mu.Lock()
	a.sessions[id] = params.Cwd
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: id}, nil
}

func (a *agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	cwd, ok := a.cwd(params.SessionId)
	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("unknown session %q", params.SessionId)
	}
	text := promptText(params)
	if strings.TrimSpace(text) == "" {
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}

	args := append([]string{}, a.claudeArgs...)
	args = append(args, "-p", "--verbose", "--output-format", "stream-json")
	if !hasArg(args, "--permission-mode") && !hasArg(args, "--permission-mode=") {
		args = append(args, "--permission-mode", "dontAsk")
	}
	args = append(args, text)

	cmd := exec.CommandContext(ctx, a.claudeCommand, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if err := cmd.Start(); err != nil {
		return acp.PromptResponse{}, err
	}

	a.setActive(params.SessionId, cmd)
	defer a.clearActive(params.SessionId, cmd)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		if err := a.handleLine(ctx, params.SessionId, scanner.Bytes()); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return acp.PromptResponse{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return acp.PromptResponse{}, err
	}
	if err := cmd.Wait(); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *agent) handleLine(ctx context.Context, sessionID acp.SessionId, line []byte) error {
	var event streamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return nil
	}
	if event.Type == "assistant" && event.Message != nil {
		for _, block := range event.Message.Content {
			if block.Type == "text" && block.Text != "" {
				if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
					SessionId: sessionID,
					Update:    acp.UpdateAgentMessageText(block.Text),
				}); err != nil {
					return err
				}
			}
		}
	}
	if event.Type == "result" && event.IsError {
		if event.Result != "" {
			_ = a.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: sessionID,
				Update:    acp.UpdateAgentMessageText(event.Result),
			})
		}
		return fmt.Errorf("claude cli returned an error")
	}
	return nil
}

func (a *agent) Cancel(_ context.Context, params acp.CancelNotification) error {
	a.mu.Lock()
	cmd := a.active[params.SessionId]
	a.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func (a *agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *agent) LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	return acp.LoadSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
}

func (a *agent) CloseSession(_ context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	a.mu.Lock()
	delete(a.sessions, params.SessionId)
	if cmd := a.active[params.SessionId]; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	delete(a.active, params.SessionId)
	a.mu.Unlock()
	return acp.CloseSessionResponse{}, nil
}

func (a *agent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (a *agent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (a *agent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, nil
}

func (a *agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, nil
}

func (a *agent) UnstableForkSession(context.Context, acp.UnstableForkSessionRequest) (acp.UnstableForkSessionResponse, error) {
	return acp.UnstableForkSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionFork)
}

func (a *agent) UnstableSetSessionModel(context.Context, acp.UnstableSetSessionModelRequest) (acp.UnstableSetSessionModelResponse, error) {
	return acp.UnstableSetSessionModelResponse{}, nil
}

func (a *agent) cwd(sessionID acp.SessionId) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cwd, ok := a.sessions[sessionID]
	return cwd, ok
}

func (a *agent) setActive(sessionID acp.SessionId, cmd *exec.Cmd) {
	a.mu.Lock()
	a.active[sessionID] = cmd
	a.mu.Unlock()
}

func (a *agent) clearActive(sessionID acp.SessionId, cmd *exec.Cmd) {
	a.mu.Lock()
	if a.active[sessionID] == cmd {
		delete(a.active, sessionID)
	}
	a.mu.Unlock()
}

func promptText(params acp.PromptRequest) string {
	var parts []string
	for _, block := range params.Prompt {
		if block.Text != nil && block.Text.Text != "" {
			parts = append(parts, block.Text.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name) {
			return true
		}
	}
	return false
}

func randomHex() string {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}
