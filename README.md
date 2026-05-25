# proxy-acp

## 1. Project Overview

`proxy-acp` bridges Zenmind `agent-platform` PROXY agents to the local Claude Code CLI through ACP. It exposes the HTTP/SSE and WebSocket surface expected by the platform, then translates requests into ACP stdio sessions.

```text
webclient -> agent-platform(PROXY) -> proxy-acp(HTTP/SSE + WS) -> claude-cli-acp -> Claude Code CLI
```

This project currently supports Claude Code CLI only. Codex CLI is not supported by this repository.

## 2. Quick Start

### Prerequisites

- Go 1.26 or newer
- Claude Code CLI available on `PATH` or configured by absolute path
- Claude credentials in the service environment, such as `ANTHROPIC_API_KEY`

### Local Run

```bash
cp .env.example .env
# Edit CLAUDE_CLI_ACP, CLAUDE_CODE_CLI, and CLAUDE_CODE_ARGS in .env when needed.
make build
make run
```

By default the service listens on `http://127.0.0.1:3210` and auth is disabled.

Point an `agent-platform` PROXY / ACP-PROXY agent at:

```yaml
mode: PROXY
proxyConfig:
  baseUrl: http://127.0.0.1:3210
```

The platform must send `params.cwd` with each query. `proxy-acp` does not choose a default working directory.

### Test

```bash
make test
```

## 3. Configuration

`proxy-acp` reads dotenv configuration from the `-env` flag, then `PROXY_ACP_ENV`, then `.env` in the working directory when present.

- `.env.example` is the committed example configuration.
- `.env` is local-only and must not be committed.
- `PROXY_ACP_PORT` defaults to `3210`.
- `PROXY_ACP_ADDR` defaults to `127.0.0.1` when empty. To allow remote access, set it explicitly, for example `PROXY_ACP_ADDR=0.0.0.0`.
- `PROXY_ACP_AUTH_TOKEN` defaults to empty, which leaves API routes unauthenticated. When set, clients must send `Authorization: Bearer <token>` or `?token=<token>`. For `agent-platform` PROXY agents, configure `proxyConfig.token` or `proxyConfig.tokenEnv` only when this token is set upstream.
- `CLAUDE_CLI_ACP` defaults to `./bin/claude-cli-acp` and may be an absolute path.
- `CLAUDE_CODE_CLI` defaults to `claude` and may be an absolute path.
- `CLAUDE_CODE_ARGS` defaults to empty and accepts shell-style argument splitting, for example `CLAUDE_CODE_ARGS="--permission-mode dontAsk"`.
- `PROXY_ACP_IDLE_TIMEOUT_MS` defaults to `1800000`.
- `agent-platform` PROXY request timeout defaults to `300000ms`; `proxyConfig.timeoutMs` is only needed when overriding that default.

Configuration priority:

```text
code defaults < dotenv file < process environment
```

Backend processes inherit the `proxy-acp` process environment, so values such as `ANTHROPIC_API_KEY`, `PATH`, and `HOME` come from the Go service process. Commands are launched directly with `exec.Command`, not through a shell, so shell aliases and functions are not inherited.

## 4. Deployment

Build local binaries:

```bash
make build
```

This writes executable artifacts to `bin/proxy-acp` and `bin/claude-cli-acp`.

Run with an explicit dotenv path:

```bash
./bin/proxy-acp -env /path/to/proxy-acp.env
```

Deployments should inject sensitive values through platform secrets or environment-specific dotenv files outside source control. This repository does not currently include a Dockerfile or container orchestration definition.

## 5. Operations

### Health Check

```bash
curl http://127.0.0.1:3210/healthz
```

### Logs

The service writes process logs to stdout/stderr. Capture those streams through the process manager, container runtime, or hosting platform used for deployment.

### Common Checks

- Confirm the configured port and address are reachable.
- Confirm `bin/claude-cli-acp` is built, `CLAUDE_CLI_ACP` points to it when customized, and the configured Claude Code CLI command exists on `PATH` or is configured by absolute path.
- Confirm `agent-platform` sends `params.cwd` for each query.
- Confirm the client sends the configured bearer token when `PROXY_ACP_AUTH_TOKEN` is set.
- Confirm long-running sessions are not being closed by `PROXY_ACP_IDLE_TIMEOUT_MS`.

When terminal or file-write capabilities are enabled, Claude Code can execute local commands or write files from the service process. Use this proxy only for trusted local/platform access.

## Compatibility Surface

- `POST /api/query` returns platform-compatible SSE with `event: message` and terminal `data: [DONE]`.
- `POST /api/submit` forwards platform HITL approval responses to ACP `session/request_permission`.
- `POST /api/interrupt` sends ACP `session/cancel`.
- `GET /ws` accepts platform PROXY frames for query, submit, and interrupt.
