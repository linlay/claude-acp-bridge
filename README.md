# proxy-acp

## 1. Project Overview

`proxy-acp` bridges Zenmind `agent-platform` PROXY agents to ACP-compatible CLI backends. It exposes the HTTP/SSE and WebSocket surface expected by the platform, then translates requests into ACP stdio sessions.

```text
webclient -> agent-platform(PROXY) -> proxy-acp(HTTP/SSE + WS) -> ACP backend
```

## 2. Quick Start

### Prerequisites

- Go 1.26 or newer
- One or more ACP-compatible backend commands available on `PATH`

### Local Run

```bash
cp config.example.yml config.yml
# Edit config.yml for local backend commands and working directories.
make run
```

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
- `authToken` is optional. When set, clients must send `Authorization: Bearer <token>` or `?token=<token>`.
- `backends[].key` selects an ACP backend. Request `params.backend` wins, then `agentKey`, then `defaultBackend`.
- `backends[].command` and `backends[].args` define the ACP stdio command.
- `backends[].defaultCwd` is the default ACP session working directory.
- `backends[].env` adds environment variables to the backend process.
- `backends[].idleTimeoutMs` controls idle session lifetime.

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
- Confirm the selected ACP backend command exists on `PATH`.
- Confirm `defaultCwd` exists and is accessible to the service process.
- Confirm the client sends the configured bearer token when `authToken` is set.
- Confirm long-running sessions are not being closed by `idleTimeoutMs`.

## Compatibility Surface

- `POST /api/query` returns platform-compatible SSE with `event: message` and terminal `data: [DONE]`.
- `POST /api/submit` forwards platform HITL approval responses to ACP `session/request_permission`.
- `POST /api/interrupt` sends ACP `session/cancel`.
- `GET /ws` accepts platform PROXY frames for query, submit, and interrupt.
