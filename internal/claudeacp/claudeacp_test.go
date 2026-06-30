package claudeacp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestParseLineEmitsPartialTextDeltas(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}}`))
	if err != nil {
		t.Fatalf("parse first delta: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("text delta should be buffered before message stop: %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`))
	if err != nil {
		t.Fatalf("parse second delta: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("second text delta should be buffered before message stop: %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`))
	if err != nil {
		t.Fatalf("parse message stop: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"hello"}) {
		t.Fatalf("message stop chunks = %#v", parsed.chunks)
	}
}

func TestParseLineSkipsFinalAssistantAfterPartialText(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`))
	if err != nil {
		t.Fatalf("parse partial: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("partial text should be buffered: %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("assistant aggregate should not flush pending chunks = %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`))
	if err != nil {
		t.Fatalf("parse message stop: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"hello"}) {
		t.Fatalf("message stop should flush pending chunks = %#v", parsed.chunks)
	}
}

func TestParseLineSkipsFinalAssistantThinkingAfterPartialText(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}}`))
	if err != nil {
		t.Fatalf("parse partial text: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("partial text should be buffered: %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","id":"late_thinking","thinking":"late reason"},{"type":"text","text":"answer"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if len(parsed.thoughts) != 0 {
		t.Fatalf("assistant aggregate should not emit late thoughts after partial text: %#v", parsed.thoughts)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("assistant aggregate should not flush pending text as content: %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`))
	if err != nil {
		t.Fatalf("parse message stop: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"answer"}) {
		t.Fatalf("message stop should flush pending text as content: %#v", parsed.chunks)
	}
}

func TestParseLineRoutesBufferedTextToThoughtWhenThinkingContinues(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me inspect first."}}}`))
	if err != nil {
		t.Fatalf("parse commentary text: %v", err)
	}
	if len(parsed.chunks) != 0 || len(parsed.thoughts) != 0 {
		t.Fatalf("commentary text should be buffered: %#v", parsed)
	}

	if _, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","id":"think_after_text"}}}`)); err != nil {
		t.Fatalf("parse thinking start: %v", err)
	}
	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Now reason."}}}`))
	if err != nil {
		t.Fatalf("parse thinking delta: %v", err)
	}
	want := []thoughtEvent{
		{id: "", text: "Let me inspect first."},
		{id: "think_after_text", text: "Now reason."},
	}
	if !reflect.DeepEqual(parsed.thoughts, want) {
		t.Fatalf("thoughts = %#v, want %#v", parsed.thoughts, want)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("continued thinking should not emit content chunks: %#v", parsed.chunks)
	}
}

func TestParseLineRoutesBufferedTextToThoughtOnToolUseStopReason(t *testing.T) {
	parser := &claudeStreamParser{}

	if _, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me inspect first."}}}`)); err != nil {
		t.Fatalf("parse commentary text: %v", err)
	}
	parsed, err := parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me inspect first."}]}}`))
	if err != nil {
		t.Fatalf("parse assistant aggregate: %v", err)
	}
	if len(parsed.chunks) != 0 || len(parsed.thoughts) != 0 {
		t.Fatalf("assistant aggregate should not flush pending text: %#v", parsed)
	}
	if _, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"tool_use"}}}`)); err != nil {
		t.Fatalf("parse message delta: %v", err)
	}
	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`))
	if err != nil {
		t.Fatalf("parse message stop: %v", err)
	}
	want := []thoughtEvent{{id: "", text: "Let me inspect first."}}
	if !reflect.DeepEqual(parsed.thoughts, want) {
		t.Fatalf("thoughts = %#v, want %#v", parsed.thoughts, want)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("tool_use stop should not emit content chunks: %#v", parsed.chunks)
	}
}

func TestParseLineEmitsThinkingDeltas(t *testing.T) {
	parser := &claudeStreamParser{}

	if _, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","id":"think_1"}}}`)); err != nil {
		t.Fatalf("parse thinking start: %v", err)
	}
	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"index":0,"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"reason"}}}`))
	if err != nil {
		t.Fatalf("parse thinking delta: %v", err)
	}
	if !reflect.DeepEqual(parsed.thoughts, []thoughtEvent{{id: "think_1", text: "reason"}}) {
		t.Fatalf("thoughts = %#v", parsed.thoughts)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"final reason"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant thinking: %v", err)
	}
	if len(parsed.thoughts) != 0 {
		t.Fatalf("assistant thinking should be skipped after partial thinking: %#v", parsed.thoughts)
	}
}

func TestParseLineRoutesThinkingToThoughtsAndTextToChunks(t *testing.T) {
	parser := &claudeStreamParser{}

	events := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","id":"think_2"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"inspect"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"text","id":"msg_1"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"final"}}}`,
	}

	parsed, err := parser.parseLine([]byte(events[0]))
	if err != nil {
		t.Fatalf("parse thinking start: %v", err)
	}
	if len(parsed.thoughts) != 0 || len(parsed.chunks) != 0 {
		t.Fatalf("start should not emit content: %#v", parsed)
	}
	parsed, err = parser.parseLine([]byte(events[1]))
	if err != nil {
		t.Fatalf("parse thinking delta: %v", err)
	}
	if !reflect.DeepEqual(parsed.thoughts, []thoughtEvent{{id: "think_2", text: "inspect"}}) {
		t.Fatalf("thoughts = %#v", parsed.thoughts)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("thinking should not emit chunks: %#v", parsed.chunks)
	}
	if _, err := parser.parseLine([]byte(events[2])); err != nil {
		t.Fatalf("parse thinking stop: %v", err)
	}
	if _, err := parser.parseLine([]byte(events[3])); err != nil {
		t.Fatalf("parse text start: %v", err)
	}
	parsed, err = parser.parseLine([]byte(events[4]))
	if err != nil {
		t.Fatalf("parse text delta: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("text delta should be buffered before message stop: %#v", parsed.chunks)
	}
	if len(parsed.thoughts) != 0 {
		t.Fatalf("text should not emit thoughts: %#v", parsed.thoughts)
	}
	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`))
	if err != nil {
		t.Fatalf("parse message stop: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"final"}) {
		t.Fatalf("message stop chunks = %#v", parsed.chunks)
	}
}

func TestParseLinePlanningModeEmitsPlanningEvents(t *testing.T) {
	parser := newClaudeStreamParser(true)

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"第一步"}}}`))
	if err != nil {
		t.Fatalf("parse plan delta: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("planning mode should not emit message chunks: %#v", parsed.chunks)
	}
	if len(parsed.planning) != 2 {
		t.Fatalf("planning events = %#v", parsed.planning)
	}
	if parsed.planning[0].eventType != "planning.start" || parsed.planning[1].eventType != "planning.delta" {
		t.Fatalf("planning events = %#v", parsed.planning)
	}
	if parsed.planning[1].planningID == "" || parsed.planning[1].text != "第一步" {
		t.Fatalf("planning delta = %#v", parsed.planning[1])
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"message_stop"}}`))
	if err != nil {
		t.Fatalf("parse message stop: %v", err)
	}
	if len(parsed.planning) != 1 || parsed.planning[0].eventType != "planning.end" {
		t.Fatalf("planning end = %#v", parsed.planning)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"不应追加"}}}`))
	if err != nil {
		t.Fatalf("parse post-end delta: %v", err)
	}
	if len(parsed.planning) != 0 {
		t.Fatalf("planning delta after end should be ignored: %#v", parsed.planning)
	}
}

func TestParseLineUsesAssistantAsFallbackWithoutPartialText(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"hello ", "world"}) {
		t.Fatalf("assistant fallback chunks = %#v", parsed.chunks)
	}
}

func TestParseLineAssistantFallbackRoutesThinkingAndText(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"thinking","id":"think_fallback","thinking":"fallback reason"},{"type":"text","text":"answer"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if !reflect.DeepEqual(parsed.thoughts, []thoughtEvent{{id: "think_fallback", text: "fallback reason"}}) {
		t.Fatalf("thoughts = %#v", parsed.thoughts)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"answer"}) {
		t.Fatalf("chunks = %#v", parsed.chunks)
	}
}

func TestParseLineReturnsResultTextBeforeError(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"result","is_error":true,"result":"boom"}`))
	if !errors.Is(err, errClaudeCLI) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"boom"}) {
		t.Fatalf("error chunks = %#v", parsed.chunks)
	}
}

func TestParseLineResultUsageSnapshotMapsClaudeTokens(t *testing.T) {
	parser := &claudeStreamParser{}

	if _, err := parser.parseLine([]byte(`{"type":"stream_event","ttft_ms":345,"event":{"type":"message_start","message":{"model":"claude-sonnet-4-6"}}}`)); err != nil {
		t.Fatalf("parse message start: %v", err)
	}
	parsed, err := parser.parseLine([]byte(`{"type":"result","duration_ms":678,"total_cost_usd":0.123,"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":70,"output_tokens":30},"modelUsage":{"claude-sonnet-4-6":{"contextWindow":200000}}}`))
	if err != nil {
		t.Fatalf("parse result usage: %v", err)
	}
	if parsed.usage == nil {
		t.Fatalf("expected usage snapshot")
	}
	parsed.usage.ReasoningEffort = "MEDIUM"
	payload := parsed.usage.platformPayload()
	usage := payload["usage"].(map[string]any)
	current := usage["current"].(map[string]any)
	if current["modelKey"] != "claude-sonnet-4-6" {
		t.Fatalf("modelKey = %#v", current["modelKey"])
	}
	if current["promptTokens"] != 190 || current["completionTokens"] != 30 || current["totalTokens"] != 220 {
		t.Fatalf("usage tokens = %#v", current)
	}
	if current["reasoningEffort"] != "MEDIUM" || current["toolCallCount"] != 0 {
		t.Fatalf("usage metadata = %#v", current)
	}
	details := current["promptTokensDetails"].(map[string]any)
	if details["cacheHitTokens"] != 70 || details["cacheMissTokens"] != 120 {
		t.Fatalf("prompt details = %#v", details)
	}
	completionDetails := current["completionTokensDetails"].(map[string]any)
	if completionDetails["reasoningTokens"] != 0 {
		t.Fatalf("completion details = %#v", completionDetails)
	}
	estimatedCost := current["estimatedCost"].(map[string]any)
	if estimatedCost["currency"] != "USD" || estimatedCost["total"] != 0.123 {
		t.Fatalf("estimated cost = %#v", estimatedCost)
	}
	timing := current["timing"].(map[string]any)
	if timing["firstTokenLatencyMs"] != 345 || timing["generationDurationMs"] != 678 {
		t.Fatalf("timing = %#v", timing)
	}
	contextWindow := payload["contextWindow"].(map[string]any)
	if contextWindow["maxSize"] != 200000 || contextWindow["currentSize"] != 190 || contextWindow["reasoningEffort"] != "MEDIUM" {
		t.Fatalf("context window = %#v", contextWindow)
	}
}

func TestParseLineResultUsageSnapshotMapsFutureClaudeReasoningTokens(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"result","usage":{"input_tokens":100,"output_tokens":30,"reasoning_output_tokens":12}}`))
	if err != nil {
		t.Fatalf("parse result usage: %v", err)
	}
	if parsed.usage == nil {
		t.Fatalf("expected usage snapshot")
	}
	payload := parsed.usage.platformPayload()
	usage := payload["usage"].(map[string]any)
	current := usage["current"].(map[string]any)
	completionDetails := current["completionTokensDetails"].(map[string]any)
	if completionDetails["reasoningTokens"] != 12 {
		t.Fatalf("completion details = %#v", completionDetails)
	}
}

func TestParseLineDetectsAskUserQuestion(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"AskUserQuestion","input":{"questions":[{"question":"你目前最希望在工作中优先改进哪个方面？","header":"工作重点","multiSelect":false,"options":[{"label":"提升代码质量"},{"label":"加快交付速度"},{"label":"深化技术能力"}]}]}}]}}`))
	if err != nil {
		t.Fatalf("parse question: %v", err)
	}
	if parsed.question == nil {
		t.Fatalf("expected question")
	}
	if parsed.question.toolUseID != "toolu_1" {
		t.Fatalf("toolUseID = %q", parsed.question.toolUseID)
	}
	questions := parsed.question.payload["questions"].([]map[string]any)
	if got := questions[0]["id"]; got != "q1" {
		t.Fatalf("question id = %#v", got)
	}
	if got := questions[0]["type"]; got != "select" {
		t.Fatalf("question type = %#v", got)
	}
	if _, ok := questions[0]["multiSelect"]; ok {
		t.Fatalf("multiSelect should be normalized away: %#v", questions[0])
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("question should not emit text chunks: %#v", parsed.chunks)
	}
}

func TestParseLineDetectsAskUserQuestionFromInputJSONDelta(t *testing.T) {
	parser := &claudeStreamParser{}

	events := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_delta","name":"AskUserQuestion","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"questions\":[{\"question\":\"工作重点\",\"multiSelect\":false,\"options\":[{\"label\":\"提升代码质量\"}"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":",{\"label\":\"加快交付速度\"},{\"label\":\"深化技术能力\"}]}]}"}}}`,
	}
	for _, event := range events {
		parsed, err := parser.parseLine([]byte(event))
		if err != nil {
			t.Fatalf("parse event: %v", err)
		}
		if parsed.question != nil {
			t.Fatalf("question emitted before stop")
		}
	}
	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`))
	if err != nil {
		t.Fatalf("parse stop: %v", err)
	}
	if parsed.question == nil {
		t.Fatalf("expected question from input_json_delta")
	}
	if parsed.question.toolUseID != "toolu_delta" {
		t.Fatalf("toolUseID = %q", parsed.question.toolUseID)
	}
	questions := parsed.question.payload["questions"].([]map[string]any)
	if got := questions[0]["type"]; got != "select" {
		t.Fatalf("question type = %#v", got)
	}
}

func TestParseLineDetectsBashToolUseFromInputJSONDelta(t *testing.T) {
	parser := &claudeStreamParser{}

	events := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git status -s\",\"description\":\"Check git status\"}"}}}`,
	}
	for _, event := range events {
		parsed, err := parser.parseLine([]byte(event))
		if err != nil {
			t.Fatalf("parse event: %v", err)
		}
		if len(parsed.toolCalls) != 0 {
			t.Fatalf("tool emitted before stop")
		}
	}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_stop","index":1}}`))
	if err != nil {
		t.Fatalf("parse stop: %v", err)
	}
	if len(parsed.toolCalls) != 1 {
		t.Fatalf("expected one tool call, got %#v", parsed.toolCalls)
	}
	call := parsed.toolCalls[0]
	if call.id != "toolu_bash" || call.name != "Bash" || call.input["command"] != "git status -s" {
		t.Fatalf("unexpected tool call %#v", call)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_start","index":2,"content_block":{"type":"text","text":"fake result"}}}`))
	if err != nil {
		t.Fatalf("parse text start: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("tool follow-up text should be suppressed before real tool result: %#v", parsed.chunks)
	}
}

func TestParseLineDetectsBashToolUseFromAssistantMessage(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git diff --stat"}}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if len(parsed.toolCalls) != 1 {
		t.Fatalf("expected tool call, got %#v", parsed.toolCalls)
	}
	if parsed.toolCalls[0].input["command"] != "git diff --stat" {
		t.Fatalf("tool input = %#v", parsed.toolCalls[0].input)
	}
}

func TestClaudeMaxToolIterationsSupportsCodeInspectionRuns(t *testing.T) {
	const minimumCodeInspectionTurns = 24
	if claudeMaxToolIterations < minimumCodeInspectionTurns {
		t.Fatalf("claude max tool iterations = %d, want at least %d for code inspection runs", claudeMaxToolIterations, minimumCodeInspectionTurns)
	}
}

func TestClaudePromptArgsIncludesPartialMessagesOnce(t *testing.T) {
	args := claudePromptArgs(nil, "hi", false)
	if got := countArg(args, "--include-partial-messages"); got != 1 {
		t.Fatalf("partial flag count = %d in %#v", got, args)
	}

	args = claudePromptArgs([]string{"--include-partial-messages"}, "hi", false)
	if got := countArg(args, "--include-partial-messages"); got != 1 {
		t.Fatalf("partial flag count with base flag = %d in %#v", got, args)
	}

	if !reflect.DeepEqual(args[len(args)-1:], []string{"hi"}) {
		t.Fatalf("prompt was not last arg: %#v", args)
	}
	if countArg(args, "--permission-mode") != 0 {
		t.Fatalf("permission mode should not be added by default: %#v", args)
	}
}

func TestAgentPromptArgsResumeRememberedClaudeSession(t *testing.T) {
	sessionID := acp.SessionId("sess_acp")
	agent := &agent{
		claudeArgs: []string{"--model", "MiniMax-M2.7"},
	}

	firstArgs := agent.promptArgs(sessionID, "first", false)
	if hasArg(firstArgs, "--resume") {
		t.Fatalf("first prompt should not resume before a Claude session is known: %#v", firstArgs)
	}
	if firstArgs[len(firstArgs)-1] != "first" {
		t.Fatalf("first prompt should remain last arg: %#v", firstArgs)
	}

	agent.rememberClaudeSession(sessionID, "claude-session-1")
	nextArgs := agent.promptArgs(sessionID, "second", false)
	if !hasArg(nextArgs, "--resume") || !argEquals(nextArgs, "claude-session-1") {
		t.Fatalf("next prompt should resume the remembered Claude session: %#v", nextArgs)
	}
	if nextArgs[len(nextArgs)-1] != "second" {
		t.Fatalf("next prompt should remain last arg: %#v", nextArgs)
	}
	if got := countArg(nextArgs, "--model"); got != 1 {
		t.Fatalf("model flag count = %d in %#v", got, nextArgs)
	}

	if _, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: sessionID}); err != nil {
		t.Fatalf("close session: %v", err)
	}
	afterCloseArgs := agent.promptArgs(sessionID, "third", false)
	if hasArg(afterCloseArgs, "--resume") {
		t.Fatalf("closed ACP session should forget Claude session: %#v", afterCloseArgs)
	}
}

func TestClaudePromptArgsPlanningModeUsesPlanPermission(t *testing.T) {
	args := claudePromptArgs(nil, "请修改文件", true)
	if got := countArg(args, "--permission-mode"); got != 1 {
		t.Fatalf("permission mode flag count = %d in %#v", got, args)
	}
	if !reflect.DeepEqual(args[len(args)-3:len(args)-1], []string{"--permission-mode", "plan"}) {
		t.Fatalf("permission mode args = %#v", args)
	}
	if !strings.Contains(args[len(args)-1], "计划模式") || !strings.Contains(args[len(args)-1], "请修改文件") {
		t.Fatalf("planning prompt = %q", args[len(args)-1])
	}
}

func TestClaudePromptArgsPlanningModeOverridesPermissionBypass(t *testing.T) {
	args := claudePromptArgs([]string{"--permission-mode", "bypassPermissions", "--dangerously-skip-permissions"}, "请修改文件", true)
	if got := countArg(args, "--permission-mode"); got != 1 {
		t.Fatalf("permission mode flag count = %d in %#v", got, args)
	}
	if got := countArg(args, "--dangerously-skip-permissions"); got != 0 {
		t.Fatalf("dangerous skip flag should be stripped in planning mode, count = %d in %#v", got, args)
	}
	if !reflect.DeepEqual(args[len(args)-3:len(args)-1], []string{"--permission-mode", "plan"}) {
		t.Fatalf("permission mode args = %#v", args)
	}
}

func TestApplyClaudeModelArgs(t *testing.T) {
	args := applyClaudeModelArgs([]string{"--permission-mode", "plan"}, "claude-sonnet-4-6", "high")
	want := []string{"--permission-mode", "plan", "--model", "claude-sonnet-4-6", "--effort", "high"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}

	promptArgs := claudePromptArgs(args, "hi", false)
	if promptArgs[len(promptArgs)-1] != "hi" {
		t.Fatalf("prompt was not last arg: %#v", promptArgs)
	}
	if got := countArg(promptArgs, "--model"); got != 1 {
		t.Fatalf("model flag count = %d in %#v", got, promptArgs)
	}
	if got := countArg(promptArgs, "--effort"); got != 1 {
		t.Fatalf("effort flag count = %d in %#v", got, promptArgs)
	}
}

func TestApplyClaudeRuntimeArgsIncludesPermissionOptions(t *testing.T) {
	args := applyClaudeRuntimeArgs([]string{"--permission-mode", "plan"}, claudeRuntimeOptions{
		model:                      "claude-opus-4-1",
		effort:                     "high",
		permissionMode:             "bypassPermissions",
		dangerouslySkipPermissions: true,
	})
	want := []string{"--permission-mode", "plan", "--model", "claude-opus-4-1", "--effort", "high", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestClaudeReasoningEffortFromArgsNormalizesForPlatform(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "separate arg", args: []string{"--effort", "medium"}, want: "MEDIUM"},
		{name: "equals arg", args: []string{"--effort=xhigh"}, want: "XHIGH"},
		{name: "unknown", args: []string{"--effort", "turbo"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeReasoningEffortFromArgs(tc.args); got != tc.want {
				t.Fatalf("effort = %q, want %q", got, tc.want)
			}
		})
	}
}

func countArg(args []string, want string) int {
	count := 0
	for _, arg := range args {
		if arg == want || strings.HasPrefix(arg, want+"=") {
			count++
		}
	}
	return count
}

func argEquals(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
