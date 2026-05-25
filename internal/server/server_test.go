package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"proxy-acp/internal/acpbridge"
	"proxy-acp/internal/config"
	"proxy-acp/internal/platform"
)

func TestQuerySSEAndSubmit(t *testing.T) {
	cfg := testConfig(t)
	manager := acpbridge.NewManager(cfg)
	defer manager.Close()
	handler := New(cfg, manager)

	server := httptest.NewServer(handler)
	defer server.Close()

	root := repoRoot(t)
	body := fmt.Sprintf(`{"requestId":"req_http","runId":"run_http","chatId":"chat_http","agentKey":"fake","message":"needs permission","params":{"cwd":%q}}`, root)
	resp, err := http.Post(server.URL+"/api/query", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post query: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content type = %q, want event stream", got)
	}

	reader := bufio.NewReader(resp.Body)
	awaitingID := ""
	var seen []string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		event, ok := readSSEEvent(t, reader)
		if !ok {
			continue
		}
		seen = append(seen, event.Type)
		if event.Type == "awaiting.ask" {
			awaitingID, _ = event.Payload["awaitingId"].(string)
			break
		}
	}
	if awaitingID == "" {
		t.Fatalf("expected awaiting.ask, seen %#v", seen)
	}

	submitBody := `{"runId":"run_http","awaitingId":"` + awaitingID + `","params":[{"id":"allow","decision":"approve"}]}`
	submitResp, err := http.Post(server.URL+"/api/submit", "application/json", strings.NewReader(submitBody))
	if err != nil {
		t.Fatalf("post submit: %v", err)
	}
	defer submitResp.Body.Close()
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d", submitResp.StatusCode)
	}

	done := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read remaining sse: %v", err)
		}
		if strings.TrimSpace(line) == "data: [DONE]" {
			done = true
			break
		}
		if strings.HasPrefix(line, "data: {") {
			event := decodeEventLine(t, line)
			seen = append(seen, event.Type)
		}
	}
	if !done {
		t.Fatalf("expected done sentinel, seen %#v", seen)
	}
	for _, want := range []string{"request.submit", "awaiting.answer", "run.complete"} {
		if !contains(seen, want) {
			t.Fatalf("missing %s, seen %#v", want, seen)
		}
	}
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) (platform.EventData, bool) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	if !strings.HasPrefix(line, "data: {") {
		return platform.EventData{}, false
	}
	return decodeEventLine(t, line), true
}

func decodeEventLine(t *testing.T, line string) platform.EventData {
	t.Helper()
	raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	var event platform.EventData
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("decode event %q: %v", raw, err)
	}
	return event
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{
		ListenAddr:     "127.0.0.1:0",
		DefaultBackend: "fake",
		Backends: []config.BackendConfig{{
			Key:     "fake",
			Command: "go",
			Args:    []string{"run", "./internal/testagent"},
		}},
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	return cfg
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return root
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
