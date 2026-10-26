# Architecture

```text
Android APK (Compose UI + device bridge + local DB)
        │  HTTPS + SSE
        ▼
      Caddy ── minis-proxy ── open-minis-server
                                   │
                                   ├─ provider APIs (OpenAI-compatible, Anthropic, Gemini)
                                   ├─ /data/sessions (append-only JSONL)
                                   ├─ /data/global (MCP config, shared server state)
                                   └─ minis-sandbox (Debian 12, root shell, PTY)
                                          └─ /workspace/sessions/<id>
                                          └─ /workspace/global
```

## Split of responsibilities

The Compose UI is unchanged. The app remains responsible for everything that
only a phone can do: Android APIs, WebView automation, sensors, notifications,
OAuth consent, audio, SAF mounts, APK installation, and the local transcript
cache used by the existing screens.

The server owns the agent:

- provider credentials and wire protocols;
- the turn loop, retries, fallbacks, empty-turn recovery and loop detection;
- the system prompt, tool bullets, prompt sections and runtime suffix;
- context estimation, compaction, summary reuse and large-result offload;
- shell and file execution in the Debian sandbox;
- the canonical session copy, replay log, cancellation and steering;
- the update proxy and release feed.

The client sends its full history on each run plus a `prompt_context` object
containing user configuration only: identity/SOUL text, enabled switches,
language/timezone, and the selected skills/MCP/memory fragments. The server
assembles every prompt byte. This keeps edits, retries and local compaction
safe without giving the app prompt authority.

## Streaming protocol

`POST /v1/agent/runs` opens an SSE response with monotonically numbered events
(`id: N`). Event types include `ready`, `text`, `thinking`, `tool_call`,
`tool_result`, `assistant_end`, `tools_end`, `usage`, `compacted`, `steered`,
`error` and `done`.

Device-bound tools pause the run with `await_client`; the app executes the
existing tool and POSTs the result back, then the same SSE response continues.
This is how browser automation, Android CLIs, memory, user questions and
sub-agents stay on the phone without duplicating them.

A dropped connection does **not** cancel the run. The app reconnects to
`GET /v1/agent/runs/{id}/events?after=N` (or `Last-Event-ID`) and resumes from
the exact event it missed. Completed runs stay replayable for a bounded window
(20 runs), and the client caches tool results so a replayed `await_client`
never repeats a device side effect.

`POST /v1/agent/runs/{id}/steer` injects a user message between turns. The
server drains pending steers at the start of the next provider call; the app's
queued-prompt UI remains the user-facing entry point.

## Sandbox and networks

The server holds its own configured provider keys, update and app tokens; the
app's provider credentials arrive with each run and are never persisted. The sandbox holds none of them;
it runs the agent's shell as root inside a dedicated **Debian 12** container,
with a real PTY, `apt`, Node, Python and build tools. There is no PRoot or
Alpine rootfs in the execution path; those files remain only for local/offline
mode and device-side file bridges.

- `/workspace/sessions/<id>/workspace` ← `/var/minis/workspace`
- `/workspace/sessions/<id>/attachments` ← `/var/minis/attachments`
- `/workspace/sessions/<id>/offloads` ← `/var/minis/offloads`
- `/workspace/sessions/<id>/browser` ← `/var/minis/browser`
- `/workspace/global/mcp-servers` ← `/var/minis/mcp-servers` (shared)
- `/workspace/global/shared` ← `/var/minis/shared` (shared)
- `/workspace/global/skills` ← `/var/minis/skills` (shared)
- `/workspace/global/memory` ← `/var/minis/memory` (shared)

Paired runs route `minis-config`, `minis-model-use` and
`minis-sessions-cli` to the phone so their full official surface (app settings,
media output, session groups) is available; `android-*`, `minis-open` and
`minis-scheduled` are device-only too. The same binaries in the sandbox call
the server's `/v1/cli/*` API for headless/scheduled/API runs, and
`minis-mcp-cli` always runs in the sandbox. The MCP client is the bundled
Python implementation.

The server joins `minis_net` + `minis_sandbox_net`; the sandbox joins only
`minis_sandbox_net`; `minis-proxy` joins `minis_net` + `internal`; Caddy joins
only `internal`. A prompt-injected shell cannot reach the VPS's other Docker
networks or host listeners. It can reach the server on `:8080` for the
`minis-*` CLI endpoints, which are scoped to the sandbox token.

Each shell command and PTY runs under `unshare -m` with
`/usr/local/bin/minis-session-exec`, which binds the session's real
`workspace`, `attachments`, `offloads` and `browser` directories, plus the
shared `mcp-servers`, `shared`, `skills` and `memory` stores, at the virtual
`/var/minis/...` paths. That makes internal file opens from Python,
Node, MCP servers and user scripts behave exactly like the official PRoot
layout instead of relying on command-string rewriting. The sandbox has
`SYS_ADMIN` + unconfined AppArmor for this, but no host mounts, devices or
Docker socket; the capability is container-local.

Device-local paths (`attachments`, `skills`, `memory`, `shared`, `mounts`,
`browser`) and commands invoking `android-*`, `minis-open` or `minis-scheduled`
are routed to the paired app. The app executes them in its existing on-device
shell exactly like the official build; the server never pretends to have
Android APIs. Interactive CLI routing is described above.

## Storage

- `/data/config.json` — providers, auth token, update token, sandbox settings
  (mode 600).
- `/data/sessions/<id>/meta.json` + `messages.jsonl` — append-only history,
  title, compaction summary/anchor.
- `/data/workspaces/<id>/` — local-mode workspace (development/tests).
- `/data/sandbox/<id>/` — persisted Debian workspace volume.
- `/data/global/` — MCP configuration and shared server state.
- `/data/scheduled.json` — server scheduler for API/headless clients.
- `/data/bin/minis-server` — last good built binary.
- `/data/go-cache`, `/data/go-mod` — startup build cache.

## Dependencies and efficiency

- One vendored Go dependency: `creack/pty` for the real terminal.
- No database driver or web framework; sessions are append-only JSONL.
- SSE replaces polling. There are no background refresh loops; the scheduler
  sleeps until the next task time and wakes on change.
- The startup build is the only compiler use; the compiled binary is cached and
  used if a later clone/build fails.
