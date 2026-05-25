# proxy-acp

## 1. Project Overview

`proxy-acp` bridges Zenmind `agent-platform` PROXY agents to ACP-compatible CLI backends. It exposes the HTTP/SSE and WebSocket surface expected by the platform, then translates requests into ACP stdio sessions.

```text
webclient -> agent-platform(PROXY) -> proxy-acp(HTTP/SSE + WS) -> ACP backend
```

## 2. Quick Start

### Prerequisites

- Go 1.26 or newer
- One or more ACP-compatible backend commands available on `PATH` or configured by absolute path
- Claude Code CLI available on `PATH` when using the local `claude-cli-acp` backend
- Claude credentials in the service environment, such as `ANTHROPIC_API_KEY`

### Local Run

```bash
cp config.example.yml config.yml
# Edit config.yml for local backend commands and working directories.
make build
make run
```

`proxy-acp` requires at least one explicitly configured backend. The example `claude-code` backend uses the repository's `claude-cli-acp` wrapper and runs the host Claude Code CLI command configured with `-claude`.

Point an `agent-platform` PROXY / ACP-PROXY agent at:

```yaml
mode: PROXY
proxyConfig:
  baseUrl: http://127.0.0.1:3210
```

### Test

```bash
make test
```

## 3. Configuration

`proxy-acp` reads YAML configuration from the `-config` flag, then `PROXY_ACP_CONFIG`, then `config.yml` or `config.yaml` in the working directory.

- `config.example.yml` is the committed example configuration.
- `config.yml` and `config.yaml` are local-only files and must not be committed.
- `authToken` is optional and defaults to empty, which leaves API routes unauthenticated. When set, clients must send `Authorization: Bearer <token>` or `?token=<token>`. For `agent-platform` PROXY agents, configure `proxyConfig.token` or `proxyConfig.tokenEnv` only when `authToken` is set upstream.
- `agent-platform` PROXY request timeout defaults to `300000ms`; `proxyConfig.timeoutMs` is only needed when overriding that default.
- `backends[].key` selects an ACP backend. Request `params.backend` wins, then `agentKey`, then `defaultBackend`.
- `backends[].command` and `backends[].args` define the ACP stdio command.
- At least one backend must be configured. `claude-code` is a conventional key for the local Claude Code wrapper, but its command and args must be explicit.
- `backends[].defaultCwd` is the default ACP session working directory.
- `backends[].env` adds environment variables to the backend process.
- `backends[].idleTimeoutMs` controls idle session lifetime.
- `backends[].capabilities.fs.readTextFile`, `backends[].capabilities.fs.writeTextFile`, and `backends[].capabilities.terminal` control what ACP client capabilities are advertised to the backend.

Claude Code defaults to file read, file write, and terminal capabilities enabled. Other backends default to file read enabled, file write disabled, and terminal disabled.

Backend processes inherit the `proxy-acp` process environment plus `backends[].env`, so values such as `ANTHROPIC_API_KEY`, `PATH`, and `HOME` come from the Go service process. Commands are launched directly with `exec.Command`, not through a shell, so shell aliases and functions are not inherited.

Configuration priority:

```text
code defaults < YAML config
```

## 4. Deployment

Build a local binary:

```bash
make build
```

Run it with an explicit configuration path:

```bash
./proxy-acp -config /path/to/config.yml
```

Deployments should inject sensitive values through platform secrets or environment-specific configuration files outside source control. This repository does not currently include a Dockerfile or container orchestration definition.

## 5. Operations

### Health Check

```bash
curl http://127.0.0.1:3210/healthz
```

### Logs

The service writes process logs to stdout/stderr. Capture those streams through the process manager, container runtime, or hosting platform used for deployment.

### Common Checks

- Confirm the configured listen address is reachable.
- Confirm the selected ACP backend command exists on `PATH` or is configured by absolute path. For the Claude wrapper backend, confirm `claude-cli-acp` is built and the configured Claude Code CLI command is available.
- Confirm `defaultCwd` exists and is accessible to the service process.
- Confirm the client sends the configured bearer token when `authToken` is set.
- Confirm long-running sessions are not being closed by `idleTimeoutMs`.

When terminal or file-write capabilities are enabled, the selected ACP backend can execute local commands or write files from the service process. Enable those capabilities only for trusted backends and trusted working directories.

## Compatibility Surface

- `POST /api/query` returns platform-compatible SSE with `event: message` and terminal `data: [DONE]`.
- `POST /api/submit` forwards platform HITL approval responses to ACP `session/request_permission`.
- `POST /api/interrupt` sends ACP `session/cancel`.
- `GET /ws` accepts platform PROXY frames for query, submit, and interrupt.
# proxy-acp
