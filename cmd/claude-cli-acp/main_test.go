package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseLineEmitsPartialTextDeltas(t *testing.T) {
	parser := &claudeStreamParser{}

	chunks, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}}`))
	if err != nil {
		t.Fatalf("parse first delta: %v", err)
	}
	if !reflect.DeepEqual(chunks, []string{"hel"}) {
		t.Fatalf("first chunks = %#v", chunks)
	}

	chunks, err = parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`))
	if err != nil {
		t.Fatalf("parse second delta: %v", err)
	}
	if !reflect.DeepEqual(chunks, []string{"lo"}) {
		t.Fatalf("second chunks = %#v", chunks)
	}
}

func TestParseLineSkipsFinalAssistantAfterPartialText(t *testing.T) {
	parser := &claudeStreamParser{}

	chunks, err := parser.parseLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`))
	if err != nil {
		t.Fatalf("parse partial: %v", err)
	}
	if !reflect.DeepEqual(chunks, []string{"hello"}) {
		t.Fatalf("partial chunks = %#v", chunks)
	}

	chunks, err = parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("assistant chunks after partial = %#v", chunks)
	}
}

func TestParseLineUsesAssistantAsFallbackWithoutPartialText(t *testing.T) {
	parser := &claudeStreamParser{}

	chunks, err := parser.parseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}}`))
	if err != nil {
		t.Fatalf("parse assistant: %v", err)
	}
	if !reflect.DeepEqual(chunks, []string{"hello ", "world"}) {
		t.Fatalf("assistant fallback chunks = %#v", chunks)
	}
}

func TestParseLineReturnsResultTextBeforeError(t *testing.T) {
	parser := &claudeStreamParser{}

	chunks, err := parser.parseLine([]byte(`{"type":"result","is_error":true,"result":"boom"}`))
	if !errors.Is(err, errClaudeCLI) {
		t.Fatalf("error = %v", err)
	}
	if !reflect.DeepEqual(chunks, []string{"boom"}) {
		t.Fatalf("error chunks = %#v", chunks)
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
