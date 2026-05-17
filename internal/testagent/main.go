package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
)

type agent struct {
	conn     *acp.AgentSideConnection
	mu       sync.Mutex
	sessions map[string]struct{}
}

func main() {
	a := &agent{sessions: map[string]struct{}{}}
	conn := acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	conn.SetLogger(slog.Default())
	a.conn = conn
	<-conn.Done()
}

func (a *agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

func (a *agent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	id := "sess_" + randomHex()
	a.mu.Lock()
	a.sessions[id] = struct{}{}
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(id)}, nil
}

func (a *agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update:    acp.UpdateAgentThoughtText("thinking"),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update:    acp.UpdateAgentMessageText("hello "),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update:    acp.UpdateAgentMessageText(promptText(params)),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update: acp.StartToolCall(
			acp.ToolCallId("tool_1"),
			"Read file",
			acp.WithStartKind(acp.ToolKindRead),
			acp.WithStartStatus(acp.ToolCallStatusPending),
			acp.WithStartRawInput(map[string]any{"path": "/tmp/demo.txt"}),
		),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: params.SessionId,
		Update: acp.UpdateToolCall(
			acp.ToolCallId("tool_1"),
			acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
			acp.WithUpdateRawOutput(map[string]any{"ok": true}),
		),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	if strings.Contains(promptText(params), "permission") {
		resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			SessionId: params.SessionId,
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: acp.ToolCallId("tool_perm"),
				Title:      acp.Ptr("Dangerous command"),
				RawInput:   map[string]any{"command": "echo ok"},
			},
			Options: []acp.PermissionOption{
				{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")},
				{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: acp.PermissionOptionId("reject")},
			},
		})
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if resp.Outcome.Selected != nil {
			if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
				SessionId: params.SessionId,
				Update:    acp.UpdateAgentMessageText(" approved"),
			}); err != nil {
				return acp.PromptResponse{}, err
			}
		}
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *agent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *agent) LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	return acp.LoadSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionLoad)
}
func (a *agent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
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

func promptText(params acp.PromptRequest) string {
	for _, block := range params.Prompt {
		if block.Text != nil {
			return block.Text.Text
		}
	}
	return ""
}

func randomHex() string {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}
