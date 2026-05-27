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
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	SessionID string `json:"session_id"`
	Event     *struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
	} `json:"event"`
	Message *struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

type claudeQuestion struct {
	toolUseID string
	payload   map[string]any
}

type partialToolUse struct {
	id      string
	name    string
	builder strings.Builder
}

type parseResult struct {
	chunks          []string
	question        *claudeQuestion
	claudeSessionID string
}

type claudeStreamParser struct {
	partialSeen bool
	question    *claudeQuestion
	sessionID   string
	suppress    bool
	tools       map[int]*partialToolUse
}

func Run(args []string) error {
	var extra multiFlag
	flags := flag.NewFlagSet("claude-cli-acp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	claudeCommand := flags.String("claude", "claude", "Claude Code CLI command")
	model := flags.String("model", "", "Claude model override")
	effort := flags.String("effort", "", "Claude effort override")
	flags.Var(&extra, "arg", "Extra argument passed to Claude Code CLI, repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	claudeArgs := applyClaudeModelArgs(extra, *model, *effort)

	a := &agent{
		claudeCommand: *claudeCommand,
		claudeArgs:    claudeArgs,
		sessions:      map[acp.SessionId]string{},
		active:        map[acp.SessionId]*exec.Cmd{},
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

func applyClaudeModelArgs(base []string, model string, effort string) []string {
	args := append([]string(nil), base...)
	if model = strings.TrimSpace(model); model != "" {
		args = append(args, "--model", model)
	}
	if effort = strings.TrimSpace(effort); effort != "" {
		args = append(args, "--effort", effort)
	}
	return args
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

	result, err := a.runClaude(ctx, params.SessionId, cwd, claudePromptArgs(a.claudeArgs, text))
	if err != nil {
		return acp.PromptResponse{}, err
	}
	if result.question != nil {
		followup, err := a.askUserQuestion(ctx, params.SessionId, result.question)
		if err != nil {
			return acp.PromptResponse{}, err
		}
		if result.claudeSessionID != "" && followup != "" {
			if _, err := a.runClaude(ctx, params.SessionId, cwd, claudeResumeArgs(a.claudeArgs, result.claudeSessionID, followup)); err != nil {
				return acp.PromptResponse{}, err
			}
		}
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *agent) runClaude(ctx context.Context, sessionID acp.SessionId, cwd string, args []string) (parseResult, error) {
	cmd := exec.CommandContext(ctx, a.claudeCommand, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
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
	parser := &claudeStreamParser{}
	var result parseResult
	for scanner.Scan() {
		parsed, err := a.handleLine(ctx, sessionID, parser, scanner.Bytes())
		if parsed.question != nil {
			result.question = parsed.question
		}
		if parsed.claudeSessionID != "" {
			result.claudeSessionID = parsed.claudeSessionID
		}
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return result, err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
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
	for _, chunk := range parsed.chunks {
		if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: sessionID,
			Update:    acp.UpdateAgentMessageText(chunk),
		}); err != nil {
			return parsed, err
		}
	}
	return parsed, err
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
	if event.Type == "stream_event" && event.Event != nil &&
		event.Event.Type == "content_block_start" &&
		event.Event.ContentBlock != nil &&
		event.Event.ContentBlock.Type == "tool_use" {
		if p.tools == nil {
			p.tools = map[int]*partialToolUse{}
		}
		p.tools[event.Event.Index] = &partialToolUse{id: event.Event.ContentBlock.ID, name: event.Event.ContentBlock.Name}
		if event.Event.ContentBlock.Name == "AskUserQuestion" {
			p.suppress = true
		}
		return result, nil
	}
	if event.Type == "stream_event" && event.Event != nil &&
		event.Event.Type == "content_block_delta" &&
		event.Event.Delta != nil &&
		event.Event.Delta.Type == "input_json_delta" {
		if tool := p.tools[event.Event.Index]; tool != nil {
			tool.builder.WriteString(event.Event.Delta.PartialJSON)
		}
		return result, nil
	}
	if event.Type == "stream_event" && event.Event != nil &&
		event.Event.Type == "content_block_stop" {
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
			}
		}
		return result, nil
	}
	if event.Type == "stream_event" && event.Event != nil &&
		event.Event.Type == "content_block_delta" &&
		event.Event.Delta != nil &&
		event.Event.Delta.Type == "text_delta" &&
		event.Event.Delta.Text != "" && !p.suppress {
		p.partialSeen = true
		result.chunks = []string{event.Event.Delta.Text}
		return result, nil
	}
	if event.Type == "assistant" && event.Message != nil {
		if question := questionFromAssistant(event.Message.Content); question != nil {
			p.question = question
			p.suppress = true
			result.question = question
			return result, nil
		}
		if p.partialSeen {
			return result, nil
		}
		var chunks []string
		for _, block := range event.Message.Content {
			if block.Type == "text" && block.Text != "" {
				chunks = append(chunks, block.Text)
			}
		}
		result.chunks = chunks
		return result, nil
	}
	if event.Type == "result" && event.IsError {
		var chunks []string
		if event.Result != "" && !p.suppress {
			chunks = append(chunks, event.Result)
		}
		result.chunks = chunks
		if p.suppress {
			return result, nil
		}
		return result, errClaudeCLI
	}
	return result, nil
}

func questionFromAssistant(blocks []struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}) *claudeQuestion {
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
	return append(args, text)
}

func claudeResumeArgs(base []string, sessionID string, text string) []string {
	args := claudePromptArgs(base, text)
	if sessionID == "" || hasArg(args, "--resume") || hasArg(args, "--resume=") || hasArg(args, "--continue") {
		return args
	}
	prompt := args[len(args)-1]
	args = args[:len(args)-1]
	args = append(args, "--resume", sessionID, prompt)
	return args
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
