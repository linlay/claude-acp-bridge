# CLAUDE.md

## 1. Project Overview

`proxy-acp` is a Go backend service that adapts Zenmind `agent-platform` PROXY traffic to ACP-compatible CLI backends. Its boundary is protocol translation and session management; it does not implement model inference, persistence, billing, or user management.

Core capabilities:

- Expose platform-compatible HTTP/SSE endpoints.
- Expose a WebSocket endpoint for platform PROXY frames.
- Provide a local `claude-cli-acp` wrapper for running the host Claude Code CLI as an ACP backend.
- Start and reuse ACP stdio backend sessions by backend, chat, and working directory.
- Forward HITL permission responses and cancellation requests to ACP sessions.

## 2. Tech Stack

- Language: Go 1.26
- HTTP server: Go standard library `net/http`
- WebSocket: `github.com/gorilla/websocket`
- ACP client SDK: `github.com/coder/acp-go-sdk`
- Config format: YAML via `gopkg.in/yaml.v3`
- Build tool: `go build` through `Makefile`
- Deployment style: process/binary deployment; container packaging is not defined in this repository.

## 3. Architecture Design

The service is split into three main layers:

- `cmd/proxy-acp`: process entrypoint, flag parsing, config loading, HTTP server lifecycle, graceful shutdown.
- `internal/server`: HTTP and WebSocket handlers, auth checks, request validation, platform response shaping.
- `internal/acpbridge`: backend selection, ACP process lifecycle, session reuse, turn execution, permission forwarding, cancellation.
- `internal/platform`: platform DTOs and HTTP/SSE serialization helpers.
- `internal/config`: YAML loading, defaults, normalization, and backend lookup.

Request flow:

```text
platform client -> internal/server -> internal/acpbridge -> ACP backend process
```

Backend sessions are keyed by backend key, chat id, and working directory. Active runs are tracked separately so interrupt requests can find the session currently handling a run.

## 4. Directory Structure

```text
cmd/proxy-acp/          Service entrypoint.
internal/acpbridge/     ACP backend session and turn management.
internal/config/        YAML config loading and normalization.
internal/platform/      Platform request/response/event types.
internal/server/        HTTP, SSE, WebSocket routes and auth.
internal/testagent/     Local test helper entrypoint.
config.example.yml      Example YAML configuration.
README.md               User-facing usage, deployment, and operations guide.
Makefile                Build, run, test, format, and cleanup commands.
```

## 5. Data Structures

Core configuration types live in `internal/config`:

- `Config`: listen address, optional auth token, default backend, backend list.
- `BackendConfig`: backend key, command, args, environment, default cwd, idle timeout.
- `BackendCapabilities`: ACP client file-system and terminal capabilities advertised to each backend.

Platform DTOs live in `internal/platform`:

- `QueryRequest`: incoming query payload and routing params.
- `SubmitRequest`: HITL permission response payload.
- `InterruptRequest`: cancellation payload.
- `EventData`: stream event envelope with ordered JSON serialization.

ACP session state lives in `internal/acpbridge`:

- `Manager`: configured backends, session registry, awaiting permission registry, active run registry.
- `backendSession`: running ACP process, ACP connection, active turn state, and lifecycle hooks.

## 6. API Definition

Authentication:

- If `authToken` is empty, API routes are unauthenticated.
- If `authToken` is set, clients must pass `Authorization: Bearer <token>` or `?token=<token>`.

Endpoints:

- `GET /healthz`: returns a JSON success response with `{ "ok": true }`.
- `POST /api/query`: accepts `platform.QueryRequest`, streams platform SSE events, and ends with `[DONE]`.
- `POST /api/submit`: accepts `platform.SubmitRequest`, forwards permission responses, and returns `SubmitResponse`.
- `POST /api/interrupt`: accepts `platform.InterruptRequest`, cancels an active ACP session run, and returns `InterruptResponse`.
- `GET /ws`: upgrades to WebSocket and accepts `request.query`, `request.submit`, and `request.interrupt` frames.

Error responses use the platform JSON envelope with non-zero `code`, a `msg`, and an empty data object.

## 7. Development Notes

- Keep protocol-specific DTOs in `internal/platform`.
- Keep ACP process and session behavior in `internal/acpbridge`.
- Keep HTTP routing, method checks, auth, and response writing in `internal/server`.
- Config defaults belong in code. Example values belong in `config.example.yml`. Local values belong in ignored `config.yml` or `config.yaml`.
- Tests should cover protocol serialization, config normalization, server handlers, and ACP bridge session behavior.
- Avoid committing real tokens, production paths, or host-specific backend credentials.

## 8. Development Flow

Typical local flow:

```bash
cp config.example.yml config.yml
make fmt
make test
make run
```

Build a binary:

```bash
make build
```

Clean local build output:

```bash
make clean
```

## 9. Known Constraints And Notes

- The repository does not currently define Docker or Kubernetes deployment assets.
- ACP backend commands must be installed separately and available to the service process.
- The `claude-code` backend should be configured explicitly, usually through the repository's `claude-cli-acp` wrapper and the host Claude Code CLI.
- Backend processes inherit the service environment plus `backends[].env`.
- Claude Code defaults to file read, file write, and terminal capabilities enabled. Other backends default to file read only.
- `config.example.yml` contains illustrative local values only and should be edited before local use.
- WebSocket origin checks currently allow all origins.
