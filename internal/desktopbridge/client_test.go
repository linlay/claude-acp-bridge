package desktopbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFromEnvDisabledWhenIncomplete(t *testing.T) {
	t.Setenv(envPluginID, "claude-acp-bridge")
	t.Setenv(envBridgeVersion, bridgeVersion)
	t.Setenv(envBridgePath, "")
	t.Setenv(envBridgeToken, "token")

	if _, ok := LoadConfigFromEnv(); ok {
		t.Fatal("expected incomplete bridge env to disable client")
	}
}

func TestClientSendsHelloAndUpsertOnAgentPlatformReady(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket bridge client is only used by the darwin plugin package")
	}
	socketDir, err := os.MkdirTemp("/tmp", "claude-acp-bridge-test-")
	if err != nil {
		t.Fatalf("create short socket dir: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := socketDir + "/plugin.sock"
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	requestCh := make(chan map[string]any, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		var hello map[string]any
		_ = json.Unmarshal(scanner.Bytes(), &hello)
		if hello["type"] != "hello" || hello["pluginId"] != "claude-acp-bridge" {
			return
		}
		_, _ = conn.Write([]byte(`{"type":"event","name":"agentPlatform.ready","createdAt":"now","data":{}}` + "\n"))
		if !scanner.Scan() {
			return
		}
		var request map[string]any
		_ = json.Unmarshal(scanner.Bytes(), &request)
		requestCh <- request
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(Config{PluginID: "claude-acp-bridge", Version: bridgeVersion, Path: socketPath, Token: "token"}, "claude", "http://127.0.0.1:17070", 300000, nil)
	client.SetAuthToken("test-bridge-auth")
	go client.Run(ctx)

	select {
	case request := <-requestCh:
		if request["method"] != "agentPlatform.upsertAcpBridge" {
			t.Fatalf("method = %v", request["method"])
		}
		params := request["params"].(map[string]any)
		if params["authToken"] != "test-bridge-auth" {
			t.Fatal("bridge auth token was not registered")
		}
		if params["bridgeId"] != "claude" || params["baseUrl"] != "http://127.0.0.1:17070" {
			t.Fatalf("unexpected params: %#v", params)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upsert request")
	}
}

func TestClientHandlesBeforeQuit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket bridge client is only used by the darwin plugin package")
	}
	socketDir, err := os.MkdirTemp("/tmp", "claude-acp-bridge-before-quit-")
	if err != nil {
		t.Fatalf("create short socket dir: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := socketDir + "/plugin.sock"
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		_, _ = conn.Write([]byte(`{"type":"event","name":"desktop.beforeQuit","createdAt":"now","data":{}}` + "\n"))
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(Config{PluginID: "claude-acp-bridge", Version: bridgeVersion, Path: socketPath, Token: "token"}, "claude", "http://127.0.0.1:17070", 300000, nil)
	go client.Run(ctx)

	select {
	case <-client.BeforeQuit():
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for beforeQuit")
	}
}

func TestClientReconnectsAfterDisconnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket bridge client is only used by the darwin plugin package")
	}
	socketDir, err := os.MkdirTemp("/tmp", "claude-acp-bridge-reconnect-")
	if err != nil {
		t.Fatalf("create short socket dir: %v", err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := socketDir + "/plugin.sock"
	firstHello := make(chan struct{}, 1)
	secondHello := make(chan struct{}, 1)

	acceptOnce := func(done chan<- struct{}) {
		_ = os.Remove(socketPath)
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Errorf("listen unix socket: %v", err)
			return
		}
		defer listener.Close()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		scanner := bufio.NewScanner(conn)
		if scanner.Scan() {
			done <- struct{}{}
		}
		_ = conn.Close()
	}

	go acceptOnce(firstHello)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(Config{PluginID: "claude-acp-bridge", Version: bridgeVersion, Path: socketPath, Token: "token"}, "claude", "http://127.0.0.1:17070", 300000, nil)
	go client.Run(ctx)

	select {
	case <-firstHello:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first hello")
	}

	go acceptOnce(secondHello)
	select {
	case <-secondHello:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reconnect hello")
	}
}

func TestRegistrationRejectionIsLoggedWithoutPrivateHostError(t *testing.T) {
	var out bytes.Buffer
	client := New(Config{}, "test", "http://127.0.0.1:17071", 300000, log.New(&out, "", 0))
	client.handleEnvelope(nil, map[string]any{"type": "response", "id": "register-1", "ok": false, "error": "private-token"})
	if !strings.Contains(out.String(), "registration rejected") || strings.Contains(out.String(), "private-token") {
		t.Fatalf("unexpected log: %s", out.String())
	}
}

func TestConnectionCancellationClosesReadLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS plugin socket")
	}
	dir, err := os.MkdirTemp("/tmp", "acp-cancel-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", dir+"/bridge.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := New(Config{Path: dir + "/bridge.sock"}, "test", "http://127.0.0.1:17071", 300000, nil)
	done := make(chan error, 1)
	go func() { done <- client.runConnection(ctx) }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("missing hello")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection read did not stop on cancellation")
	}
}
