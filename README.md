# claude-acp-bridge

## 1. Project Overview

`claude-acp-bridge` bridges Zenmind `agent-platform` traffic to the local Claude Code CLI through ACP. It exposes the HTTP/SSE and WebSocket surface expected by the platform, then translates requests into ACP stdio sessions backed by Claude Code CLI.

```text
webclient -> agent-platform -> claude-acp-bridge(HTTP/SSE + WS + internal ACP backend) -> Claude Code CLI
```

This project currently supports Claude Code CLI only.

## 2. Quick Start

### Prerequisites

- Go 1.26 or newer
- Claude Code CLI available on `PATH` or configured by absolute path
- Claude credentials in the service environment, such as `ANTHROPIC_API_KEY`

### Local Run

```bash
cp .env.example .env
# Edit CLAUDE_CODE_CLI and CLAUDE_CODE_ARGS in .env when needed.
make build
make run
```

By default the service listens on `http://127.0.0.1:17070` and auth is disabled.

For a direct PROXY agent, point `agent-platform` at:

```yaml
mode: PROXY
proxyConfig:
  baseUrl: http://127.0.0.1:17070
```

The platform must send `params.cwd` with each query. `claude-acp-bridge` does not choose a default working directory.

### Test

```bash
make test
```

## 3. Configuration

`claude-acp-bridge` reads dotenv configuration from the `-env` flag, then `CLAUDE_ACP_BRIDGE_ENV`, then legacy `PROXY_ACP_ENV`, then `.env` in the working directory when present.

- `.env.example` is the committed example configuration.
- `.env` is local-only and must not be committed.
- `CLAUDE_ACP_BRIDGE_PORT` defaults to `17070`.
- `CLAUDE_ACP_BRIDGE_ADDR` defaults to `127.0.0.1` when empty. To allow remote access, set it explicitly, for example `CLAUDE_ACP_BRIDGE_ADDR=0.0.0.0`.
- `CLAUDE_ACP_BRIDGE_AUTH_TOKEN` defaults to empty, which leaves API routes unauthenticated. When set, clients must send `Authorization: Bearer <token>` or `?token=<token>`.
- `CLAUDE_CODE_CLI` defaults to `claude` and may be an absolute path.
- `CLAUDE_CODE_ARGS` defaults to empty and accepts shell-style argument splitting, for example `CLAUDE_CODE_ARGS="--permission-mode dontAsk"`.
- When the platform sends `model.modelId` or `model.key`, this service appends `--model <model>` for that Claude Code CLI session. When the platform sends `model.reasoningEffort` as `LOW`, `MEDIUM`, `HIGH`, `EXTRA_HIGH`, or `MAX`, this service appends `--effort <low|medium|high|xhigh|max>`.
- `http_proxy`, `HTTP_PROXY`, `https_proxy`, and `HTTPS_PROXY` default to empty. When set in the dotenv file or process environment, non-empty values are passed to the Claude Code CLI backend.
- `CLAUDE_ACP_BRIDGE_IDLE_TIMEOUT_MS` defaults to `1800000`.
- Legacy `PROXY_ACP_PORT`, `PROXY_ACP_ADDR`, `PROXY_ACP_AUTH_TOKEN`, and `PROXY_ACP_IDLE_TIMEOUT_MS` are still accepted as fallbacks during migration.

Configuration priority:

```text
code defaults < dotenv file < process environment
```

Backend processes inherit the `claude-acp-bridge` process environment, so values such as `ANTHROPIC_API_KEY`, `PATH`, and `HOME` come from the Go service process. Proxy variables configured through dotenv are overlaid onto the backend environment before launching Claude Code CLI. Commands are launched directly with `exec.Command`, not through a shell, so shell aliases and functions are not inherited.

## 4. Zenmind Desktop Integration

This project matches the Zenmind Desktop plugin bridge shape:

1. Build or import the `make release-plugin` archive.
2. Initialize and start `claude-acp-bridge` in Desktop.
3. When Desktop emits `desktop.ready`, `agentPlatform.ready`, or `service.statusChanged:agent-platform`, the plugin sends `agentPlatform.upsertAcpBridge` to register:

```yaml
acp-bridges:
  claude:
    base-url: http://127.0.0.1:17070
    timeout-ms: 300000
```

4. Point the target agent at `runtimeConfig.acpBridgeId: claude`.

If the Desktop bridge environment variables are absent, the binary still runs as a standalone local bridge and does not modify `agent-platform` configuration.

## 5. Deployment

Build the local binary:

```bash
make build
```

This writes the single executable artifact to `bin/claude-acp-bridge`.

Build the Windows amd64 binary:

```bash
make build-windows-amd64
```

This writes `dist/windows-amd64/claude-acp-bridge.exe`.

Build the Desktop plugin archive:

```bash
make release-plugin
```

This writes `dist/release/claude-acp-bridge-v0.1.0-darwin-arm64.tar.gz`.

Run with an explicit dotenv path:

```bash
./bin/claude-acp-bridge -env /path/to/claude-acp-bridge.env
```

Deployments should inject sensitive values through platform secrets or environment-specific dotenv files outside source control.

## 6. Operations

### Health Check

```bash
curl http://127.0.0.1:17070/healthz
```

### Logs

The service writes process logs to stdout/stderr. In Desktop plugin bundles, lifecycle scripts write runtime logs to `run/claude-acp-bridge.log` unless `SERVICE_LOG_DIR` overrides the location.

### Common Checks

- Confirm the configured port and address are reachable.
- Confirm the configured Claude Code CLI command exists on `PATH` or is configured by absolute path.
- Confirm `agent-platform` sends `params.cwd` for each query.
- Confirm the client sends the configured bearer token when `CLAUDE_ACP_BRIDGE_AUTH_TOKEN` is set.
- Confirm long-running sessions are not being closed by `CLAUDE_ACP_BRIDGE_IDLE_TIMEOUT_MS`.

When terminal or file-write capabilities are enabled, Claude Code can execute local commands or write files from the service process. Use this bridge only for trusted local/platform access.

## Compatibility Surface

- `POST /api/query` returns platform-compatible SSE with `event: message` and terminal `data: [DONE]`.
- `POST /api/submit` forwards platform HITL approval responses to ACP `session/request_permission`.
- `POST /api/steer` queues a follow-up user prompt on the active ACP run.
- `POST /api/interrupt` sends ACP `session/cancel`.
- `GET /ws` accepts platform frames for query, submit, steer, access-level updates, and interrupt.
