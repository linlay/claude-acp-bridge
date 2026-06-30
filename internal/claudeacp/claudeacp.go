package claudeacp

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
	"path/filepath"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"

	"proxy-acp/internal/claudecli"
	"proxy-acp/internal/platform"
)

var errClaudeCLI = errors.New("claude cli returned an error")

const claudeMaxToolIterations = 32

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

	mu             sync.Mutex
	sessions       map[acp.SessionId]string
	claudeSessions map[acp.SessionId]string
	active         map[acp.SessionId]*exec.Cmd
}

type streamEvent struct {
	Type         string                      `json:"type"`
	Subtype      string                      `json:"subtype"`
	Result       string                      `json:"result"`
	IsError      bool                        `json:"is_error"`
	SessionID    string                      `json:"session_id"`
	TotalCostUSD float64                     `json:"total_cost_usd"`
	DurationMS   int64                       `json:"duration_ms"`
	TTFTMS       int                         `json:"ttft_ms"`
	Usage        *claudeUsage                `json:"usage"`
	ModelUsage   map[string]claudeModelUsage `json:"modelUsage"`
	Event        *struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type     string         `json:"type"`
			ID       string         `json:"id"`
			Name     string         `json:"name"`
			Text     string         `json:"text"`
			Thinking string         `json:"thinking"`
			Input    map[string]any `json:"input"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Message *struct {
			Model string       `json:"model"`
			Usage *claudeUsage `json:"usage"`
		} `json:"message"`
		Usage *claudeUsage `json:"usage"`
	} `json:"event"`
	Message *struct {
		Content []claudeContentBlock `json:"content"`
		Model   string               `json:"model"`
		Usage   *claudeUsage         `json:"usage"`
	} `json:"message"`
}

type claudeContentBlock struct {
	Type     string         `json:"type"`
	Text     string         `json:"text"`
	Thinking string         `json:"thinking"`
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Input    map[string]any `json:"input"`
}

type claudeUsage struct {
	InputTokens              int                  `json:"input_tokens"`
	OutputTokens             int                  `json:"output_tokens"`
	CacheCreationInputTokens int                  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                  `json:"cache_read_input_tokens"`
	ReasoningTokens          int                  `json:"reasoning_tokens"`
	ReasoningOutputTokens    int                  `json:"reasoning_output_tokens"`
	ThinkingTokens           int                  `json:"thinking_tokens"`
	ServerToolUse            *claudeServerToolUse `json:"server_tool_use"`
}

type claudeServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
	WebFetchRequests  int `json:"web_fetch_requests"`
}

type claudeModelUsage struct {
	InputTokens              int     `json:"inputTokens"`
	OutputTokens             int     `json:"outputTokens"`
	CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
	ContextWindow            int     `json:"contextWindow"`
	MaxOutputTokens          int     `json:"maxOutputTokens"`
}

type claudeQuestion struct {
	toolUseID string
	payload   map[string]any
}

type claudeToolCall struct {
	id    string
	name  string
	input map[string]any
}

type claudeToolExecutionResult struct {
	id        string
	name      string
	output    string
	errorText string
	exitCode  *int
	truncated bool
}

type partialToolUse struct {
	id      string
	name    string
	input   map[string]any
	builder strings.Builder
}

type partialContentBlock struct {
	id        string
	blockType string
}

type pendingTextBlock struct {
	id      string
	builder strings.Builder
}

type parseResult struct {
	chunks          []string
	thoughts        []thoughtEvent
	planning        []planningEvent
	usage           *claudeUsageSnapshot
	question        *claudeQuestion
	toolCalls       []claudeToolCall
	claudeSessionID string
}

type thoughtEvent struct {
	id   string
	text string
}

type planningEvent struct {
	eventType  string
	planningID string
	text       string
}

type claudeUsageSnapshot struct {
	ModelKey             string
	ReasoningEffort      string
	ContextWindow        int
	Usage                claudeUsage
	CostUSD              float64
	ToolCallCount        int
	FirstTokenLatencyMS  int
	GenerationDurationMS int64
}

type claudeStreamParser struct {
	partialSeen         bool
	thoughtPartialSeen  bool
	toolPartialSeen     bool
	question            *claudeQuestion
	sessionID           string
	modelKey            string
	firstTokenLatencyMS int
	toolUseCount        int
	suppress            bool
	planningMode        bool
	planningID          string
	planningStarted     bool
	planningEnded       bool
	messageStopReason   string
	blocks              map[int]partialContentBlock
	pendingText         *pendingTextBlock
	tools               map[int]*partialToolUse
}

func newClaudeStreamParser(planningMode bool) *claudeStreamParser {
	return &claudeStreamParser{
		planningMode: planningMode,
		planningID:   "claude_plan_" + randomHex(),
	}
}

func Run(args []string) error {
	var extra multiFlag
	flags := flag.NewFlagSet("claude-cli-acp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	claudeCommand := flags.String("claude", "claude", "Claude Code CLI command")
	model := flags.String("model", "", "Claude model override")
	effort := flags.String("effort", "", "Claude effort override")
	permissionMode := flags.String("permission-mode", "", "Claude permission mode override")
	dangerouslySkipPermissions := flags.Bool("dangerously-skip-permissions", false, "Bypass Claude Code permission checks")
	flags.Var(&extra, "arg", "Extra argument passed to Claude Code CLI, repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	claudeArgs := applyClaudeRuntimeArgs(extra, claudeRuntimeOptions{
		model:                      *model,
		effort:                     *effort,
		permissionMode:             *permissionMode,
		dangerouslySkipPermissions: *dangerouslySkipPermissions,
	})

	a := &agent{
		claudeCommand:  *claudeCommand,
		claudeArgs:     claudeArgs,
		sessions:       map[acp.SessionId]string{},
		claudeSessions: map[acp.SessionId]string{},
		active:         map[acp.SessionId]*exec.Cmd{},
	}
	conn := acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	conn.SetLogger(slog.Default())
	a.conn = conn
	<-conn.Done()
	return nil
}

func (a *agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
}

type claudeRuntimeOptions struct {
	model                      string
	effort                     string
	permissionMode             string
	dangerouslySkipPermissions bool
}

func applyClaudeModelArgs(base []string, model string, effort string) []string {
	return applyClaudeRuntimeArgs(base, claudeRuntimeOptions{model: model, effort: effort})
}

func applyClaudeRuntimeArgs(base []string, opts claudeRuntimeOptions) []string {
	return claudecli.ApplyRuntimeArgs(base, claudecli.RuntimeOptions{
		Model:                      opts.model,
		Effort:                     opts.effort,
		PermissionMode:             opts.permissionMode,
		DangerouslySkipPermissions: opts.dangerouslySkipPermissions,
	})
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

	planningMode := promptPlanningMode(params)
	args := a.promptArgs(params.SessionId, text, planningMode)
	for iteration := 0; iteration < claudeMaxToolIterations; iteration++ {
		result, err := a.runClaude(ctx, params.SessionId, cwd, args, planningMode)
		a.rememberClaudeSession(params.SessionId, result.claudeSessionID)
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if result.question != nil {
			followup, err := a.askUserQuestion(ctx, params.SessionId, result.question)
			if err != nil {
				return acp.PromptResponse{}, err
			}
			if result.claudeSessionID == "" || followup == "" {
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}
			args = claudeResumeArgs(a.claudeArgs, result.claudeSessionID, followup, planningMode)
			continue
		}
		if len(result.toolCalls) > 0 {
			followup, err := a.executeClaudeToolCalls(ctx, params.SessionId, cwd, result.toolCalls)
			if err != nil {
				return acp.PromptResponse{}, err
			}
			if result.claudeSessionID == "" || followup == "" {
				return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
			}
			args = claudeResumeArgs(a.claudeArgs, result.claudeSessionID, followup, planningMode)
			continue
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}
	return acp.PromptResponse{}, fmt.Errorf("claude tool loop exceeded maximum iterations (%d)", claudeMaxToolIterations)
}

func (a *agent) executeClaudeToolCalls(ctx context.Context, sessionID acp.SessionId, cwd string, calls []claudeToolCall) (string, error) {
	results := make([]claudeToolExecutionResult, 0, len(calls))
	for _, call := range calls {
		result, err := a.executeClaudeToolCall(ctx, sessionID, cwd, call)
		if err != nil {
			return "", err
		}
		results = append(results, result)
	}
	return claudeToolResultFollowup(results), nil
}

func (a *agent) executeClaudeToolCall(ctx context.Context, sessionID acp.SessionId, cwd string, call claudeToolCall) (claudeToolExecutionResult, error) {
	switch {
	case strings.EqualFold(call.name, "Bash"):
		return a.executeClaudeBashTool(ctx, sessionID, cwd, call)
	case strings.EqualFold(call.name, "Read"):
		return a.executeClaudeReadTool(ctx, sessionID, cwd, call)
	case strings.EqualFold(call.name, "Write"):
		return a.executeClaudeWriteTool(ctx, sessionID, cwd, call)
	}
	result := claudeToolExecutionResult{id: call.id, name: call.name, errorText: "Unsupported Claude tool: " + call.name}
	_ = a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindOther)
	_ = a.sendClaudeToolDone(ctx, sessionID, result)
	return result, nil
}

func (a *agent) executeClaudeReadTool(ctx context.Context, sessionID acp.SessionId, cwd string, call claudeToolCall) (claudeToolExecutionResult, error) {
	path, err := claudeToolPath(cwd, call.input)
	if err != nil {
		result := claudeToolExecutionResult{id: call.id, name: call.name, errorText: err.Error()}
		_ = a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindRead)
		_ = a.sendClaudeToolDone(ctx, sessionID, result)
		return result, nil
	}
	call.input["file_path"] = path
	if err := a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindRead); err != nil {
		return claudeToolExecutionResult{}, err
	}
	req := acp.ReadTextFileRequest{SessionId: sessionID, Path: path}
	if line, ok := intFromAny(call.input["offset"]); ok && line > 0 {
		req.Line = &line
	}
	if limit, ok := intFromAny(call.input["limit"]); ok && limit > 0 {
		req.Limit = &limit
	}
	resp, err := a.conn.ReadTextFile(ctx, req)
	result := claudeToolExecutionResult{id: call.id, name: call.name, output: resp.Content}
	if err != nil {
		result.errorText = err.Error()
	}
	if err := a.sendClaudeToolDone(ctx, sessionID, result); err != nil {
		return claudeToolExecutionResult{}, err
	}
	return result, nil
}

func (a *agent) executeClaudeWriteTool(ctx context.Context, sessionID acp.SessionId, cwd string, call claudeToolCall) (claudeToolExecutionResult, error) {
	path, err := claudeToolPath(cwd, call.input)
	if err != nil {
		result := claudeToolExecutionResult{id: call.id, name: call.name, errorText: err.Error()}
		_ = a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindEdit)
		_ = a.sendClaudeToolDone(ctx, sessionID, result)
		return result, nil
	}
	content := stringFromAny(call.input["content"])
	call.input["file_path"] = path
	call.input["reason"] = "Write file: " + path
	if err := a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindEdit); err != nil {
		return claudeToolExecutionResult{}, err
	}
	if err := a.requestClaudeWriteApproval(ctx, sessionID, call); err != nil {
		result := claudeToolExecutionResult{id: call.id, name: call.name, errorText: err.Error()}
		_ = a.sendClaudeToolDone(ctx, sessionID, result)
		return result, nil
	}
	_, err = a.conn.WriteTextFile(ctx, acp.WriteTextFileRequest{SessionId: sessionID, Path: path, Content: content})
	result := claudeToolExecutionResult{id: call.id, name: call.name, output: "wrote " + path}
	if err != nil {
		result.errorText = err.Error()
	}
	if err := a.sendClaudeToolDone(ctx, sessionID, result); err != nil {
		return claudeToolExecutionResult{}, err
	}
	return result, nil
}

func (a *agent) requestClaudeWriteApproval(ctx context.Context, sessionID acp.SessionId, call claudeToolCall) error {
	title := claudeToolTitle(call)
	kind := acp.ToolKindEdit
	status := acp.ToolCallStatusPending
	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sessionID,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(call.id),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			RawInput:   call.input,
		},
		Options: []acp.PermissionOption{
			{OptionId: "allow", Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "reject", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	})
	if err != nil {
		return err
	}
	if resp.Outcome.Selected != nil && resp.Outcome.Selected.OptionId == "allow" {
		return nil
	}
	return fmt.Errorf("write rejected by user")
}

func (a *agent) executeClaudeBashTool(ctx context.Context, sessionID acp.SessionId, cwd string, call claudeToolCall) (claudeToolExecutionResult, error) {
	command := strings.TrimSpace(stringFromAny(call.input["command"]))
	if command == "" {
		result := claudeToolExecutionResult{id: call.id, name: call.name, errorText: "Bash tool call missing command"}
		_ = a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindExecute)
		_ = a.sendClaudeToolDone(ctx, sessionID, result)
		return result, nil
	}
	if err := a.sendClaudeToolStart(ctx, sessionID, call, acp.ToolKindExecute); err != nil {
		return claudeToolExecutionResult{}, err
	}
	limit := 1024 * 1024
	resp, err := a.conn.CreateTerminal(ctx, acp.CreateTerminalRequest{
		SessionId:       sessionID,
		Command:         "bash",
		Args:            []string{"-lc", command},
		Cwd:             &cwd,
		OutputByteLimit: &limit,
	})
	if err != nil {
		result := claudeToolExecutionResult{id: call.id, name: call.name, errorText: err.Error()}
		_ = a.sendClaudeToolDone(ctx, sessionID, result)
		return result, nil
	}
	if err := a.sendClaudeToolRunning(ctx, sessionID, call, resp.TerminalId); err != nil {
		return claudeToolExecutionResult{}, err
	}
	status, waitErr := a.conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{SessionId: sessionID, TerminalId: resp.TerminalId})
	output, outputErr := a.conn.TerminalOutput(ctx, acp.TerminalOutputRequest{SessionId: sessionID, TerminalId: resp.TerminalId})
	_, _ = a.conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{SessionId: sessionID, TerminalId: resp.TerminalId})

	result := claudeToolExecutionResult{id: call.id, name: call.name, output: output.Output, truncated: output.Truncated}
	if status.ExitCode != nil {
		result.exitCode = status.ExitCode
	}
	if waitErr != nil {
		result.errorText = waitErr.Error()
	} else if outputErr != nil {
		result.errorText = outputErr.Error()
	}
	if err := a.sendClaudeToolDone(ctx, sessionID, result); err != nil {
		return claudeToolExecutionResult{}, err
	}
	return result, nil
}

func (a *agent) sendClaudeToolStart(ctx context.Context, sessionID acp.SessionId, call claudeToolCall, kind acp.ToolKind) error {
	title := claudeToolTitle(call)
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.StartToolCall(acp.ToolCallId(call.id), title,
			acp.WithStartKind(kind),
			acp.WithStartStatus(acp.ToolCallStatusPending),
			acp.WithStartRawInput(call.input),
		),
	})
}

func (a *agent) sendClaudeToolRunning(ctx context.Context, sessionID acp.SessionId, call claudeToolCall, terminalID string) error {
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.UpdateToolCall(acp.ToolCallId(call.id),
			acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
			acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolTerminalRef(terminalID)}),
		),
	})
}

func (a *agent) sendClaudeToolDone(ctx context.Context, sessionID acp.SessionId, result claudeToolExecutionResult) error {
	status := acp.ToolCallStatusCompleted
	if strings.TrimSpace(result.errorText) != "" || (result.exitCode != nil && *result.exitCode != 0) {
		status = acp.ToolCallStatusFailed
	}
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.UpdateToolCall(acp.ToolCallId(result.id),
			acp.WithUpdateStatus(status),
			acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock(result.summary()))}),
			acp.WithUpdateRawOutput(result.rawOutput()),
		),
	})
}

func (r claudeToolExecutionResult) summary() string {
	var parts []string
	if r.output != "" {
		parts = append(parts, r.output)
	}
	if r.exitCode != nil {
		parts = append(parts, fmt.Sprintf("exitCode=%d", *r.exitCode))
	}
	if r.truncated {
		parts = append(parts, "output truncated")
	}
	if strings.TrimSpace(r.errorText) != "" {
		parts = append(parts, "error: "+r.errorText)
	}
	if len(parts) == 0 {
		return "(no output)"
	}
	return strings.Join(parts, "\n")
}

func (r claudeToolExecutionResult) rawOutput() map[string]any {
	out := map[string]any{
		"output":    r.output,
		"truncated": r.truncated,
	}
	if r.exitCode != nil {
		out["exitCode"] = *r.exitCode
	}
	if strings.TrimSpace(r.errorText) != "" {
		out["error"] = r.errorText
	}
	return out
}

func claudeToolResultFollowup(results []claudeToolExecutionResult) string {
	if len(results) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("系统已执行你刚才请求的工具调用。请只基于以下真实工具结果继续回答；不要声称未执行，也不要要求用户手动运行同一命令。\n")
	for _, result := range results {
		b.WriteString("\n<tool_result id=\"")
		b.WriteString(result.id)
		b.WriteString("\" name=\"")
		b.WriteString(result.name)
		b.WriteString("\">\n")
		b.WriteString(result.summary())
		b.WriteString("\n</tool_result>\n")
	}
	return b.String()
}

func claudeToolTitle(call claudeToolCall) string {
	if strings.EqualFold(call.name, "Bash") {
		if command := strings.TrimSpace(stringFromAny(call.input["command"])); command != "" {
			return "Bash: " + command
		}
	}
	if strings.EqualFold(call.name, "Read") || strings.EqualFold(call.name, "Write") {
		if path := strings.TrimSpace(stringFromAny(call.input["file_path"])); path != "" {
			return call.name + ": " + path
		}
	}
	if strings.TrimSpace(call.name) != "" {
		return call.name
	}
	return "Claude tool"
}

func claudeToolPath(cwd string, input map[string]any) (string, error) {
	path := strings.TrimSpace(stringFromAny(input["file_path"]))
	if path == "" {
		path = strings.TrimSpace(stringFromAny(input["path"]))
	}
	if path == "" {
		return "", fmt.Errorf("tool call missing file_path")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	base := strings.TrimSpace(cwd)
	if base == "" {
		return "", fmt.Errorf("cwd is required for relative path %q", path)
	}
	return filepath.Clean(filepath.Join(base, path)), nil
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func intFromAny(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		n, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
}

func ensureClaudeToolID(id string, name string) string {
	id = strings.TrimSpace(id)
	if id != "" {
		return id
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	return name
}

func cloneToolInput(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	return cloneMap(input)
}

func claudeToolCallFromBlock(block claudeContentBlock) (claudeToolCall, bool) {
	if block.Type != "tool_use" || block.Name == "AskUserQuestion" {
		return claudeToolCall{}, false
	}
	return claudeToolCall{
		id:    ensureClaudeToolID(block.ID, block.Name),
		name:  block.Name,
		input: cloneToolInput(block.Input),
	}, true
}

func claudeToolCallsFromAssistant(blocks []claudeContentBlock) []claudeToolCall {
	var calls []claudeToolCall
	for _, block := range blocks {
		if call, ok := claudeToolCallFromBlock(block); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func claudeToolCallFromPartial(tool *partialToolUse) (claudeToolCall, bool) {
	if tool == nil || tool.name == "AskUserQuestion" {
		return claudeToolCall{}, false
	}
	input := cloneToolInput(tool.input)
	if partialJSON := strings.TrimSpace(tool.builder.String()); partialJSON != "" {
		if err := json.Unmarshal([]byte(partialJSON), &input); err != nil {
			input = map[string]any{}
		}
	}
	return claudeToolCall{
		id:    ensureClaudeToolID(tool.id, tool.name),
		name:  tool.name,
		input: cloneToolInput(input),
	}, true
}

func (a *agent) promptArgs(sessionID acp.SessionId, text string, planningMode bool) []string {
	if claudeSessionID := a.claudeSession(sessionID); claudeSessionID != "" {
		return claudeResumeArgs(a.claudeArgs, claudeSessionID, text, planningMode)
	}
	return claudePromptArgs(a.claudeArgs, text, planningMode)
}

func (a *agent) rememberClaudeSession(sessionID acp.SessionId, claudeSessionID string) {
	claudeSessionID = strings.TrimSpace(claudeSessionID)
	if claudeSessionID == "" {
		return
	}
	a.mu.Lock()
	if a.claudeSessions == nil {
		a.claudeSessions = map[acp.SessionId]string{}
	}
	a.claudeSessions[sessionID] = claudeSessionID
	a.mu.Unlock()
}

func (a *agent) claudeSession(sessionID acp.SessionId) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.claudeSessions[sessionID])
}

func promptPlanningMode(params acp.PromptRequest) bool {
	if params.Meta == nil {
		return false
	}
	enabled, _ := params.Meta[platform.PromptMetaPlanningMode].(bool)
	return enabled
}

func (a *agent) runClaude(ctx context.Context, sessionID acp.SessionId, cwd string, args []string, planningMode bool) (parseResult, error) {
	cmd := exec.CommandContext(ctx, a.claudeCommand, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	prepareChildProcess(cmd)
	cmd.Cancel = func() error {
		return terminateChildProcess(cmd)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return parseResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return parseResult{}, err
	}

	a.setActive(sessionID, cmd)
	defer a.clearActive(sessionID, cmd)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	parser := newClaudeStreamParser(planningMode)
	var result parseResult
	for scanner.Scan() {
		parsed, err := a.handleLine(ctx, sessionID, parser, scanner.Bytes())
		if parsed.question != nil {
			result.question = parsed.question
		}
		if len(parsed.toolCalls) > 0 {
			result.toolCalls = append(result.toolCalls, parsed.toolCalls...)
		}
		if parsed.claudeSessionID != "" {
			result.claudeSessionID = parsed.claudeSessionID
		}
		if err != nil {
			_ = terminateChildProcess(cmd)
			_ = cmd.Wait()
			return result, err
		}
		if result.question != nil || len(result.toolCalls) > 0 {
			_ = terminateChildProcess(cmd)
			_ = cmd.Wait()
			return result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		_ = terminateChildProcess(cmd)
		_ = cmd.Wait()
		return result, err
	}
	if err := cmd.Wait(); err != nil {
		if result.question != nil {
			return result, nil
		}
		return result, err
	}
	return result, nil
}

func (a *agent) handleLine(ctx context.Context, sessionID acp.SessionId, parser *claudeStreamParser, line []byte) (parseResult, error) {
	parsed, err := parser.parseLine(line)
	for _, thought := range parsed.thoughts {
		if err := a.sendThoughtText(ctx, sessionID, thought.id, thought.text); err != nil {
			return parsed, err
		}
	}
	for _, planning := range parsed.planning {
		if err := a.sendPlanningEvent(ctx, sessionID, planning.eventType, planning.planningID, planning.text); err != nil {
			return parsed, err
		}
	}
	for _, chunk := range parsed.chunks {
		if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sessionID,
			Update:    acp.UpdateAgentMessageText(chunk),
		}); err != nil {
			return parsed, err
		}
	}
	if parsed.usage != nil {
		if err := a.sendUsageSnapshot(ctx, sessionID, *parsed.usage); err != nil {
			return parsed, err
		}
	}
	return parsed, err
}

func (a *agent) sendThoughtText(ctx context.Context, sessionID acp.SessionId, _ string, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	update := acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
			Content:       acp.TextBlock(text),
			SessionUpdate: "agent_thought_chunk",
		},
	}
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update:    update,
	})
}

func (a *agent) sendPlanningEvent(ctx context.Context, sessionID acp.SessionId, eventType string, planningID string, text string) error {
	messageID := planningID
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
			Content:       acp.TextBlock(text),
			MessageId:     &messageID,
			SessionUpdate: "agent_thought_chunk",
			Meta: map[string]any{
				platform.ACPMetaEventType:  eventType,
				platform.ACPMetaPlanningID: planningID,
			},
		}},
	})
}

func (a *agent) sendUsageSnapshot(ctx context.Context, sessionID acp.SessionId, usage claudeUsageSnapshot) error {
	if strings.TrimSpace(usage.ReasoningEffort) == "" {
		usage.ReasoningEffort = claudeReasoningEffortFromArgs(a.claudeArgs)
	}
	payload := usage.platformPayload()
	if len(payload) == 0 {
		return nil
	}
	return a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sessionID,
		Update: acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
			Content:       acp.TextBlock(""),
			SessionUpdate: "agent_thought_chunk",
			Meta: map[string]any{
				platform.ACPMetaEventType:     "usage.snapshot",
				platform.ACPMetaUsageSnapshot: payload,
			},
		}},
	})
}

func (a *agent) askUserQuestion(ctx context.Context, sessionID acp.SessionId, question *claudeQuestion) (string, error) {
	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sessionID,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(question.toolUseID),
			Title:      acp.Ptr("AskUserQuestion"),
			RawInput:   question.payload,
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Submit answer", OptionId: acp.PermissionOptionId("submitted")},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Cancel", OptionId: acp.PermissionOptionId("cancelled")},
		},
	})
	if err != nil {
		return "", err
	}
	if resp.Outcome.Cancelled != nil {
		return "用户取消了该问题。", nil
	}
	if resp.Outcome.Selected == nil {
		return "", nil
	}
	if text, _ := resp.Outcome.Selected.Meta["answerText"].(string); strings.TrimSpace(text) != "" {
		return "用户回答了以下问题：\n" + text, nil
	}
	return "用户已经回答了该问题，请继续。", nil
}

func (p *claudeStreamParser) parseLine(line []byte) (parseResult, error) {
	var event streamEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return parseResult{}, nil
	}
	result := parseResult{}
	if event.SessionID != "" {
		p.sessionID = event.SessionID
		result.claudeSessionID = event.SessionID
	}
	if event.TTFTMS > 0 && p.firstTokenLatencyMS == 0 {
		p.firstTokenLatencyMS = event.TTFTMS
	}

	if event.Type == "stream_event" && event.Event != nil {
		if event.Event.Message != nil {
			p.setModel(event.Event.Message.Model)
		}
		switch event.Event.Type {
		case "content_block_start":
			if event.Event.ContentBlock != nil {
				p.setContentBlock(event.Event.Index, event.Event.ContentBlock.ID, event.Event.ContentBlock.Type)
			}
			if event.Event.ContentBlock != nil && event.Event.ContentBlock.Type == "thinking" && event.Event.ContentBlock.Thinking != "" {
				result.thoughts = append(result.thoughts, p.flushPendingTextAsThoughts()...)
				p.thoughtPartialSeen = true
				result.thoughts = append(result.thoughts, thoughtEvent{id: p.contentBlockID(event.Event.Index), text: event.Event.ContentBlock.Thinking})
			}
			if event.Event.ContentBlock != nil && event.Event.ContentBlock.Type == "text" && event.Event.ContentBlock.Text != "" && !p.suppress {
				p.partialSeen = true
				if p.planningMode {
					result.planning = p.planningDelta(event.Event.ContentBlock.Text)
				} else {
					p.appendPendingText(p.contentBlockID(event.Event.Index), event.Event.ContentBlock.Text)
				}
			}
			if event.Event.ContentBlock != nil && event.Event.ContentBlock.Type == "tool_use" {
				result.thoughts = append(result.thoughts, p.flushPendingTextAsThoughts()...)
				p.toolPartialSeen = true
				p.toolUseCount++
				if p.tools == nil {
					p.tools = map[int]*partialToolUse{}
				}
				p.tools[event.Event.Index] = &partialToolUse{
					id:    event.Event.ContentBlock.ID,
					name:  event.Event.ContentBlock.Name,
					input: cloneToolInput(event.Event.ContentBlock.Input),
				}
				if event.Event.ContentBlock.Name == "AskUserQuestion" {
					p.suppress = true
				}
				if event.Event.ContentBlock.Name != "AskUserQuestion" {
					p.suppress = true
				}
			}
			return result, nil
		case "content_block_delta":
			if event.Event.Delta == nil {
				return result, nil
			}
			switch event.Event.Delta.Type {
			case "":
				if stopReason := strings.TrimSpace(event.Event.Delta.StopReason); stopReason != "" {
					p.messageStopReason = stopReason
				}
			case "input_json_delta":
				if tool := p.tools[event.Event.Index]; tool != nil {
					tool.builder.WriteString(event.Event.Delta.PartialJSON)
				}
			case "thinking_delta":
				if event.Event.Delta.Thinking != "" {
					result.thoughts = append(result.thoughts, p.flushPendingTextAsThoughts()...)
					p.thoughtPartialSeen = true
					result.thoughts = append(result.thoughts, thoughtEvent{id: p.contentBlockID(event.Event.Index), text: event.Event.Delta.Thinking})
				}
			case "text_delta":
				if event.Event.Delta.Text != "" && !p.suppress {
					p.partialSeen = true
					if p.planningMode {
						result.planning = p.planningDelta(event.Event.Delta.Text)
					} else {
						p.appendPendingText(p.contentBlockID(event.Event.Index), event.Event.Delta.Text)
					}
				}
			}
			return result, nil
		case "message_delta":
			if event.Event.Delta != nil {
				p.messageStopReason = strings.TrimSpace(event.Event.Delta.StopReason)
			}
			return result, nil
		case "content_block_stop":
			if tool := p.tools[event.Event.Index]; tool != nil {
				delete(p.tools, event.Event.Index)
				if tool.name == "AskUserQuestion" {
					var input map[string]any
					if err := json.Unmarshal([]byte(tool.builder.String()), &input); err == nil {
						payload := normalizeQuestionPayload(input)
						if len(payload) > 0 {
							question := &claudeQuestion{toolUseID: tool.id, payload: payload}
							p.question = question
							result.question = question
						}
					}
				} else if call, ok := claudeToolCallFromPartial(tool); ok {
					result.toolCalls = append(result.toolCalls, call)
				}
			}
			p.clearContentBlock(event.Event.Index)
			return result, nil
		case "message_stop":
			if p.messageStopReason == "tool_use" {
				result.thoughts = append(result.thoughts, p.flushPendingTextAsThoughts()...)
			} else {
				result.chunks = append(result.chunks, p.flushPendingTextAsChunks()...)
			}
			p.messageStopReason = ""
			result.planning = p.finishPlanning()
			return result, nil
		}
	}

	if event.Type == "assistant" && event.Message != nil {
		p.setModel(event.Message.Model)
		if !p.toolPartialSeen {
			p.toolUseCount += countToolUseBlocks(event.Message.Content)
		}
		if question := questionFromAssistant(event.Message.Content); question != nil {
			result.thoughts = append(result.thoughts, p.flushPendingTextAsThoughts()...)
			p.question = question
			p.suppress = true
			result.question = question
			return result, nil
		}
		if calls := claudeToolCallsFromAssistant(event.Message.Content); len(calls) > 0 {
			result.thoughts = append(result.thoughts, p.flushPendingTextAsThoughts()...)
			p.suppress = true
			result.toolCalls = calls
			return result, nil
		}
		if p.hasPendingText() {
			return result, nil
		}
		if p.hasStreamedAssistantContent() {
			return result, nil
		}
		var chunks []string
		for _, block := range event.Message.Content {
			switch block.Type {
			case "thinking":
				if block.Thinking != "" && !p.thoughtPartialSeen {
					result.thoughts = append(result.thoughts, thoughtEvent{id: block.ID, text: block.Thinking})
				}
			case "text":
				if block.Text == "" || p.partialSeen {
					continue
				}
				if p.planningMode {
					result.planning = append(result.planning, p.planningDelta(block.Text)...)
				} else {
					chunks = append(chunks, block.Text)
				}
			}
		}
		result.chunks = chunks
		return result, nil
	}
	if event.Type == "result" {
		if event.Usage != nil {
			result.usage = p.usageSnapshot(event)
		}
		result.chunks = append(result.chunks, p.flushPendingTextAsChunks()...)
		result.planning = append(result.planning, p.finishPlanning()...)
		var chunks []string
		if event.Result != "" && !p.suppress && event.IsError {
			if p.planningMode {
				result.planning = append(result.planning, p.planningDelta(event.Result)...)
				result.planning = append(result.planning, p.finishPlanning()...)
			} else {
				chunks = append(chunks, event.Result)
			}
		}
		result.chunks = append(result.chunks, chunks...)
		if p.suppress || !event.IsError {
			return result, nil
		}
		return result, errClaudeCLI
	}
	return result, nil
}

func (p *claudeStreamParser) hasStreamedAssistantContent() bool {
	return p.partialSeen || p.thoughtPartialSeen || p.planningStarted
}

func (p *claudeStreamParser) appendPendingText(id string, text string) {
	if text == "" {
		return
	}
	if p.pendingText == nil {
		p.pendingText = &pendingTextBlock{id: strings.TrimSpace(id)}
	}
	if p.pendingText.id == "" {
		p.pendingText.id = strings.TrimSpace(id)
	}
	p.pendingText.builder.WriteString(text)
}

func (p *claudeStreamParser) hasPendingText() bool {
	return p.pendingText != nil && p.pendingText.builder.Len() > 0
}

func (p *claudeStreamParser) flushPendingTextAsThoughts() []thoughtEvent {
	if !p.hasPendingText() {
		return nil
	}
	pending := p.pendingText
	p.pendingText = nil
	p.thoughtPartialSeen = true
	return []thoughtEvent{{id: pending.id, text: pending.builder.String()}}
}

func (p *claudeStreamParser) flushPendingTextAsChunks() []string {
	if !p.hasPendingText() {
		return nil
	}
	pending := p.pendingText
	p.pendingText = nil
	return []string{pending.builder.String()}
}

func (p *claudeStreamParser) setContentBlock(index int, id string, blockType string) {
	if p.blocks == nil {
		p.blocks = map[int]partialContentBlock{}
	}
	p.blocks[index] = partialContentBlock{id: strings.TrimSpace(id), blockType: strings.TrimSpace(blockType)}
}

func (p *claudeStreamParser) clearContentBlock(index int) {
	delete(p.blocks, index)
}

func (p *claudeStreamParser) contentBlockID(index int) string {
	if block, ok := p.blocks[index]; ok {
		if block.id != "" {
			return block.id
		}
		if block.blockType != "" {
			return fmt.Sprintf("claude_%s_%d", block.blockType, index)
		}
	}
	return ""
}

func questionFromAssistant(blocks []claudeContentBlock) *claudeQuestion {
	for _, block := range blocks {
		if block.Type != "tool_use" || block.Name != "AskUserQuestion" {
			continue
		}
		payload := normalizeQuestionPayload(block.Input)
		if len(payload) == 0 {
			return nil
		}
		id := block.ID
		if id == "" {
			id = "AskUserQuestion"
		}
		return &claudeQuestion{toolUseID: id, payload: payload}
	}
	return nil
}

func countToolUseBlocks(blocks []claudeContentBlock) int {
	count := 0
	for _, block := range blocks {
		if block.Type == "tool_use" {
			count++
		}
	}
	return count
}

func (p *claudeStreamParser) setModel(model string) {
	if model = strings.TrimSpace(model); model != "" {
		p.modelKey = model
	}
}

func (p *claudeStreamParser) planningDelta(text string) []planningEvent {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if p.planningEnded {
		return nil
	}
	events := []planningEvent{}
	if !p.planningStarted {
		p.planningStarted = true
		p.planningEnded = false
		events = append(events, planningEvent{eventType: "planning.start", planningID: p.planningID})
	}
	events = append(events, planningEvent{eventType: "planning.delta", planningID: p.planningID, text: text})
	return events
}

func (p *claudeStreamParser) finishPlanning() []planningEvent {
	if !p.planningMode || !p.planningStarted || p.planningEnded {
		return nil
	}
	p.planningEnded = true
	return []planningEvent{{eventType: "planning.end", planningID: p.planningID}}
}

func (p *claudeStreamParser) usageSnapshot(event streamEvent) *claudeUsageSnapshot {
	if event.Usage == nil {
		return nil
	}
	modelKey := p.modelKey
	contextWindow := 0
	for model, usage := range event.ModelUsage {
		if strings.TrimSpace(modelKey) == "" {
			modelKey = strings.TrimSpace(model)
		}
		if strings.TrimSpace(model) == strings.TrimSpace(modelKey) || len(event.ModelUsage) == 1 {
			if usage.ContextWindow > 0 {
				contextWindow = usage.ContextWindow
			}
			break
		}
	}
	return &claudeUsageSnapshot{
		ModelKey:             modelKey,
		ContextWindow:        contextWindow,
		Usage:                *event.Usage,
		CostUSD:              event.TotalCostUSD,
		ToolCallCount:        p.usageToolCallCount(*event.Usage),
		FirstTokenLatencyMS:  p.firstTokenLatencyMS,
		GenerationDurationMS: event.DurationMS,
	}
}

func (p *claudeStreamParser) usageToolCallCount(usage claudeUsage) int {
	if p.toolUseCount > 0 {
		return p.toolUseCount
	}
	if usage.ServerToolUse == nil {
		return 0
	}
	return maxInt(usage.ServerToolUse.WebSearchRequests, 0) + maxInt(usage.ServerToolUse.WebFetchRequests, 0)
}

func (u claudeUsageSnapshot) platformPayload() map[string]any {
	promptTokens := u.Usage.InputTokens + u.Usage.CacheCreationInputTokens + u.Usage.CacheReadInputTokens
	completionTokens := u.Usage.OutputTokens
	reasoningTokens := claudeReasoningTokens(u.Usage)
	totalTokens := promptTokens + completionTokens
	if totalTokens <= 0 {
		return nil
	}
	current := map[string]any{
		"promptTokens":           maxInt(promptTokens, 0),
		"completionTokens":       maxInt(completionTokens, 0),
		"totalTokens":            maxInt(totalTokens, 0),
		"llmChatCompletionCount": 1,
		"toolCallCount":          maxInt(u.ToolCallCount, 0),
	}
	if modelKey := strings.TrimSpace(u.ModelKey); modelKey != "" {
		current["modelKey"] = modelKey
	}
	if effort := strings.TrimSpace(u.ReasoningEffort); effort != "" {
		current["reasoningEffort"] = effort
	}
	if promptTokens > 0 || u.Usage.CacheReadInputTokens > 0 {
		current["promptTokensDetails"] = map[string]any{
			"cacheHitTokens":  maxInt(u.Usage.CacheReadInputTokens, 0),
			"cacheMissTokens": maxInt(u.Usage.InputTokens+u.Usage.CacheCreationInputTokens, 0),
		}
	}
	current["completionTokensDetails"] = map[string]any{
		"reasoningTokens": reasoningTokens,
	}
	if u.CostUSD > 0 {
		current["estimatedCost"] = map[string]any{
			"currency": "USD",
			"total":    u.CostUSD,
		}
	}
	if u.FirstTokenLatencyMS > 0 || u.GenerationDurationMS > 0 {
		timing := map[string]any{}
		if u.FirstTokenLatencyMS > 0 {
			timing["firstTokenLatencyMs"] = u.FirstTokenLatencyMS
		}
		if u.GenerationDurationMS > 0 {
			timing["generationDurationMs"] = int(u.GenerationDurationMS)
		}
		current["timing"] = timing
	}

	payload := map[string]any{
		"usage": map[string]any{"current": current},
	}
	contextWindow := map[string]any{}
	if u.ContextWindow > 0 {
		contextWindow["maxSize"] = u.ContextWindow
	}
	if promptTokens > 0 {
		contextWindow["currentSize"] = maxInt(promptTokens, 0)
	}
	if modelKey := strings.TrimSpace(u.ModelKey); modelKey != "" {
		contextWindow["modelKey"] = modelKey
		payload["model"] = map[string]any{"key": modelKey}
	}
	if effort := strings.TrimSpace(u.ReasoningEffort); effort != "" {
		contextWindow["reasoningEffort"] = effort
	}
	if len(contextWindow) > 0 {
		payload["contextWindow"] = contextWindow
	}
	return payload
}

func claudeReasoningTokens(usage claudeUsage) int {
	for _, value := range []int{
		usage.ReasoningOutputTokens,
		usage.ReasoningTokens,
		usage.ThinkingTokens,
	} {
		if value > 0 {
			return value
		}
	}
	return 0
}

func maxInt(value int, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func normalizeQuestionPayload(input map[string]any) map[string]any {
	questions, ok := sliceOfMaps(input["questions"])
	if !ok || len(questions) == 0 {
		return nil
	}
	outQuestions := make([]map[string]any, 0, len(questions))
	for idx, question := range questions {
		q := cloneMap(question)
		id, _ := q["id"].(string)
		if strings.TrimSpace(id) == "" {
			id = fmt.Sprintf("q%d", idx+1)
			q["id"] = id
		}
		if _, ok := q["type"].(string); !ok {
			if _, hasOptions := q["options"]; hasOptions {
				if boolValue(q["multiSelect"]) || boolValue(q["multiple"]) {
					q["type"] = "multi-select"
				} else {
					q["type"] = "select"
				}
			}
		}
		delete(q, "multiSelect")
		delete(q, "multiple")
		if options, ok := sliceOfMaps(q["options"]); ok {
			normalizedOptions := make([]map[string]any, 0, len(options))
			for optionIdx, option := range options {
				o := cloneMap(option)
				if optID, _ := o["id"].(string); strings.TrimSpace(optID) == "" {
					o["id"] = fmt.Sprintf("%s_o%d", id, optionIdx+1)
				}
				normalizedOptions = append(normalizedOptions, o)
			}
			q["options"] = normalizedOptions
		}
		outQuestions = append(outQuestions, q)
	}
	return map[string]any{
		"mode":      "question",
		"questions": outQuestions,
	}
}

func sliceOfMaps(value any) ([]map[string]any, bool) {
	switch typed := value.(type) {
	case []map[string]any:
		return typed, true
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, m)
		}
		return out, true
	default:
		return nil, false
	}
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func boolValue(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func (a *agent) Cancel(_ context.Context, params acp.CancelNotification) error {
	a.mu.Lock()
	cmd := a.active[params.SessionId]
	a.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = terminateChildProcess(cmd)
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
	delete(a.claudeSessions, params.SessionId)
	if cmd := a.active[params.SessionId]; cmd != nil && cmd.Process != nil {
		_ = terminateChildProcess(cmd)
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

func claudePromptArgs(base []string, text string, planningMode bool) []string {
	args := append([]string{}, base...)
	if planningMode {
		args = withoutClaudePermissionBypass(args)
	}
	args = append(args, "-p", "--verbose", "--output-format", "stream-json")
	if !hasArg(args, "--include-partial-messages") {
		args = append(args, "--include-partial-messages")
	}
	if planningMode {
		args = append(args, "--permission-mode", "plan")
	}
	if planningMode {
		text = planningPrompt(text)
	}
	return append(args, text)
}

func withoutClaudePermissionBypass(args []string) []string {
	return claudecli.WithoutPermissionBypass(args)
}

func claudeResumeArgs(base []string, sessionID string, text string, planningMode bool) []string {
	args := claudePromptArgs(base, text, planningMode)
	if sessionID == "" || hasArg(args, "--resume") || hasArg(args, "--resume=") || hasArg(args, "--continue") {
		return args
	}
	prompt := args[len(args)-1]
	args = args[:len(args)-1]
	args = append(args, "--resume", sessionID, prompt)
	return args
}

func planningPrompt(text string) string {
	return "你现在处于计划模式。请只输出一份清晰、可执行的实施计划，先不要修改文件、不要执行命令、不要调用工具。计划需要覆盖关键步骤、风险和验证方式。\n\n用户需求：\n" + strings.TrimSpace(text)
}

func claudeReasoningEffortFromArgs(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "--effort" && i+1 < len(args) {
			return normalizeClaudeReasoningEffortForPlatform(args[i+1])
		}
		if strings.HasPrefix(arg, "--effort=") {
			return normalizeClaudeReasoningEffortForPlatform(strings.TrimPrefix(arg, "--effort="))
		}
	}
	return ""
}

func normalizeClaudeReasoningEffortForPlatform(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	case "high":
		return "HIGH"
	case "xhigh", "extra_high":
		return "XHIGH"
	case "max":
		return "MAX"
	default:
		return ""
	}
}

func hasArg(args []string, name string) bool {
	return claudecli.HasArg(args, name)
}

func randomHex() string {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b[:])
}
