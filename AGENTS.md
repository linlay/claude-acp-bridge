# AGENTS.md

## 1. Project Overview

`claude-acp-bridge` is a Go backend service that adapts Zenmind `agent-platform` traffic to Claude Code CLI through ACP. Its boundary is protocol translation and session management; it does not implement model inference, persistence, billing, or user management.

Core capabilities:

- Expose platform-compatible HTTP/SSE endpoints.
- Expose a WebSocket endpoint for platform frames.
- Run an internal ACP backend mode for the host Claude Code CLI from the same `claude-acp-bridge` binary.
- Register the `claude` ACP bridge automatically when launched as a Zenmind Desktop plugin.
- Start and reuse ACP stdio backend sessions by backend, chat, working directory, and runtime model options.
- Forward HITL permission responses, steering prompts, access-level updates, and cancellation requests to ACP sessions.

Codex CLI is not supported by this repository.

## 2. Tech Stack

- Language: Go 1.26
- HTTP server: Go standard library `net/http`
- WebSocket: `github.com/gorilla/websocket`
- ACP client SDK: `github.com/coder/acp-go-sdk`
- Config format: dotenv loaded by `internal/config`
- Build tool: `go build` through `Makefile`
- Deployment style: process/binary deployment and Zenmind Desktop plugin bundle

## 3. Architecture Design

The service is split into these main layers:

- `cmd/claude-acp-bridge`: process entrypoint, flag parsing, env config loading, HTTP server lifecycle, Desktop bridge lifecycle, graceful shutdown.
- `internal/claudeacp`: internal ACP stdio backend mode for running Claude Code CLI.
- `internal/desktopbridge`: optional Desktop socket client that upserts the `claude` ACP bridge.
- `internal/server`: HTTP and WebSocket handlers, auth checks, request validation, platform response shaping.
- `internal/acpbridge`: backend selection, ACP process lifecycle, session reuse, turn execution, permission forwarding, cancellation.
- `internal/platform`: platform DTOs and HTTP/SSE serialization helpers.
- `internal/config`: dotenv loading, defaults, legacy env fallback, normalization, and backend lookup.

Request flow:

```text
platform client -> internal/server -> internal/acpbridge -> claude-acp-bridge hidden ACP mode -> Claude Code CLI
```

Backend sessions are keyed by backend key, chat id, working directory, and runtime model options. Active runs are tracked separately so interrupt and steering requests can find the session currently handling a run.

## 4. Directory Structure

```text
cmd/claude-acp-bridge/ Service entrypoint.
internal/acpbridge/    ACP backend session and turn management.
internal/claudeacp/    Internal Claude Code ACP backend.
internal/claudecli/    Claude Code CLI argument helpers.
internal/config/       Dotenv config loading and normalization.
internal/desktopbridge/Desktop plugin bridge client.
internal/platform/     Platform request/response/event types.
internal/server/       HTTP, SSE, WebSocket routes and auth.
internal/testagent/    Local test helper entrypoint.
scripts/               Desktop plugin lifecycle helpers.
manifest.json          Zenmind Desktop plugin manifest.
.env.example           Example dotenv configuration.
README.md              User-facing usage, deployment, and operations guide.
Makefile               Build, run, test, format, and release commands.
```

## 5. Data Structures

Core configuration types live in `internal/config`:

- `Config`: listen address, optional auth token, default backend, backend list.
- `BackendConfig`: backend key, command, args, environment, idle timeout, and capabilities.
- `BackendCapabilities`: ACP client file-system and terminal capabilities advertised to Claude.

Platform DTOs live in `internal/platform`:

- `QueryRequest`: incoming query payload and routing params.
- `SubmitRequest`: HITL permission response payload.
- `SteerRequest`: follow-up prompt payload for active runs.
- `InterruptRequest`: cancellation payload.
- `EventData`: stream event envelope with ordered JSON serialization.

ACP session state lives in `internal/acpbridge`:

- `Manager`: configured backend, session registry, awaiting permission registry, active run registry.
- `backendSession`: running ACP process, ACP connection, active turn state, and lifecycle hooks.

## 6. API Definition

Authentication:

- If `CLAUDE_ACP_BRIDGE_AUTH_TOKEN` is empty, API routes are unauthenticated.
- If `CLAUDE_ACP_BRIDGE_AUTH_TOKEN` is set, clients must pass `Authorization: Bearer <token>` or `?token=<token>`.
- Legacy `PROXY_ACP_AUTH_TOKEN` remains a fallback during migration.

Endpoints:

- `GET /healthz`: returns a JSON success response with `{ "ok": true }`.
- `GET /api/models`: probes Claude Code CLI and returns a platform model catalog.
- `POST /api/query`: accepts `platform.QueryRequest`, requires `params.cwd`, streams platform SSE events, and ends with `[DONE]`.
- `POST /api/submit`: accepts `platform.SubmitRequest`, forwards permission responses, and returns `SubmitResponse`.
- `POST /api/access-level`: accepts `platform.AccessLevelRequest`, updates active-run access policy, and returns `AccessLevelResponse`.
- `POST /api/steer`: accepts `platform.SteerRequest`, queues a follow-up prompt on the active run, and returns `SteerResponse`.
- `POST /api/interrupt`: accepts `platform.InterruptRequest`, cancels an active ACP session run, and returns `InterruptResponse`.
- `GET /ws`: upgrades to WebSocket and accepts matching `request.*` frames.

Error responses use the platform JSON envelope with non-zero `code`, a `msg`, and an empty data object.

## 7. Development Notes

- Keep protocol-specific DTOs in `internal/platform`.
- Keep ACP process and session behavior in `internal/acpbridge`.
- Keep HTTP routing, method checks, auth, and response writing in `internal/server`.
- Keep Desktop plugin socket behavior in `internal/desktopbridge`.
- Config defaults belong in code. Example values belong in `.env.example`. Local values belong in ignored `.env` files.
- The platform owns the session working directory and must send `params.cwd`; do not add a service-side default cwd.
- Tests should cover protocol serialization, config loading, Desktop bridge registration, server handlers, and ACP bridge session behavior.
- Avoid committing real tokens, production paths, or host-specific backend credentials.

## 8. Development Flow

Typical local flow:

```bash
cp .env.example .env
make fmt
make test
make run
```

Build the local binary:

```bash
make build
```

Build the Desktop plugin archive:

```bash
make release-plugin
```

Clean local build output:

```bash
make clean
```

## 9. Known Constraints And Notes

- Claude Code CLI must be installed separately and available to the service process.
- `CLAUDE_CODE_CLI` configures the host Claude Code CLI command or absolute path.
- `CLAUDE_CODE_ARGS` configures extra Claude Code CLI arguments using shell-style splitting.
- `CLAUDE_ACP_BRIDGE_ADDR` defaults to local-only `127.0.0.1`; remote access requires an explicit value such as `0.0.0.0`.
- `CLAUDE_ACP_BRIDGE_PORT` defaults to `17070`.
- Backend processes inherit the service environment.
- Claude defaults to file read, file write, and terminal capabilities enabled.
- WebSocket origin checks currently allow all origins.

## macOS Desktop plugin release

`make release-plugin` builds a Darwin binary and a single-root ZIP with `pluginApiVersion: 1`. `ARCH=amd64` selects Intel Macs; default is arm64. Use only canonical `agentPlatform.upsertAcpBridge`, `bridgeId`, `acp-bridges`, and `runtimeConfig.acpBridgeId`. Windows plugin delivery is deferred. Never package real `.env` or runtime state.
