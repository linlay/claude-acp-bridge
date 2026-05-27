package claudeacp

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseLineEmitsPartialTextDeltas(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}}`))
	if err != nil {
		t.Fatalf("parse first delta: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"hel"}) {
		t.Fatalf("first chunks = %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`))
	if err != nil {
		t.Fatalf("parse second delta: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"lo"}) {
		t.Fatalf("second chunks = %#v", parsed.chunks)
	}
}

func TestParseLineSkipsFinalAssistantAfterPartialText(t *testing.T) {
	parser := &claudeStreamParser{}

	parsed, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`))
	if err != nil {
		t.Fatalf("parse partial: %v", err)
	}
	if !reflect.DeepEqual(parsed.chunks, []string{"hello"}) {
		t.Fatalf("partial chunks = %#v", parsed.chunks)
	}

	parsed, err = parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if len(parsed.chunks) != 0 {
		t.Fatalf("assistant chunks after partial = %#v", parsed.chunks)
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

func TestClaudePromptArgsIncludesPartialMessagesOnce(t *testing.T) {
	args := claudePromptArgs(nil, "hi")
	if got := countArg(args, "--include-partial-messages"); got != 1 {
		t.Fatalf("partial flag count = %d in %#v", got, args)
	}

	args = claudePromptArgs([]string{"--include-partial-messages"}, "hi")
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

func TestApplyClaudeModelArgs(t *testing.T) {
	args := applyClaudeModelArgs([]string{"--permission-mode", "plan"}, "claude-sonnet-4-6", "high")
	want := []string{"--permission-mode", "plan", "--model", "claude-sonnet-4-6", "--effort", "high"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}

	promptArgs := claudePromptArgs(args, "hi")
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

func countArg(args []string, want string) int {
	count := 0
	for _, arg := range args {
		if arg == want || strings.HasPrefix(arg, want+"=") {
			count++
		}
	}
	return count
}
