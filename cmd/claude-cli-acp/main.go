package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

var errClaudeCLI = errors.New("claude cli returned an error")

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
	Event   *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type claudeStreamParser struct {
	partialSeen bool
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

	cmd := exec.CommandContext(ctx, a.claudeCommand, claudePromptArgs(a.claudeArgs, text)...)
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
	parser := &claudeStreamParser{}
	for scanner.Scan() {
		if err := a.handleLine(ctx, params.SessionId, parser, scanner.Bytes()); err != nil {
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

func (a *agent) handleLine(ctx context.Context, sessionID acp.SessionId, parser *claudeStreamParser, line []byte) error {
	chunks, err := parser.parseLine(line)
	for _, chunk := range chunks {
		if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sessionID,
			Update:    acp.UpdateAgentMessageText(chunk),
		}); err != nil {
			return err
		}
	}
	return err
}

func (p *claudeStreamParser) parseLine(line []byte) ([]string, error) {
	var event streamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return nil, nil
	}
	if event.Type == "stream_event" && event.Event != nil &&
		event.Event.Type == "content_block_delta" &&
		event.Event.Delta != nil &&
		event.Event.Delta.Type == "text_delta" &&
		event.Event.Delta.Text != "" {
		p.partialSeen = true
		return []string{event.Event.Delta.Text}, nil
	}
	if event.Type == "assistant" && event.Message != nil {
		if p.partialSeen {
			return nil, nil
		}
		var chunks []string
		for _, block := range event.Message.Content {
			if block.Type == "text" && block.Text != "" {
				chunks = append(chunks, block.Text)
			}
		}
		return chunks, nil
	}
	if event.Type == "result" && event.IsError {
		var chunks []string
		if event.Result != "" {
			chunks = append(chunks, event.Result)
		}
		return chunks, errClaudeCLI
	}
	return nil, nil
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

func claudePromptArgs(base []string, text string) []string {
	args := append([]string{}, base...)
	args = append(args, "-p", "--verbose", "--output-format", "stream-json")
	if !hasArg(args, "--include-partial-messages") {
		args = append(args, "--include-partial-messages")
	}
	if !hasArg(args, "--permission-mode") && !hasArg(args, "--permission-mode=") {
		args = append(args, "--permission-mode", "dontAsk")
	}
	return append(args, text)
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
