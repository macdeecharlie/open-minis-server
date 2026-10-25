# Architecture

```text
Android APK (GUI + device bridge) ── HTTPS/SSE ── Caddy ── open-minis-server
                                                              │
                                                              ├─ provider APIs (OpenAI-compatible / Anthropic)
                                                              ├─ /data/sessions (JSONL)
                                                              ├─ /data/workspaces (per-session sandbox)
                                                              └─ GitHub private releases (update proxy)
```

## Why this shape

The app's existing ChatViewModel is the UI state machine: it owns the
transcript, tool cards, session list and device-bound side effects. Rewriting
that into a second UI would break the "identical interface" requirement. The
server therefore owns the parts that are not UI:

- provider credentials and wire protocols;
- the agent turn loop and tool dispatch;
- the Linux sandbox and file tools;
- usage accounting and the canonical session copy.

The app sends its complete history for each run. That makes an edit, retry,
compaction or truncation on the client safe: the server never has to reconcile
a divergent transcript. The server stores the same turns for the future Web UI.

## Streaming protocol

One `POST /v1/agent/runs` opens an SSE response. Events are ordered and typed
(`text`, `tool_call`, `tool_result`, `assistant_end`, `tools_end`, `done`).
Device-bound tools pause the run with `await_client`; the app executes the
existing tool and POSTs the result back, then the same SSE response continues.
This is the only way browser automation, native integrations, memory and
sub-agents stay on the phone without duplicating them.

## Sandbox

Each session maps to `/data/workspaces/<session>` with an `attachments`,
`offloads` and `browser` subdirectory. Shell commands run as `minis` in the
workspace with a process-group kill on cancel, a timeout and a 200 KB output
cap. File tools are path-confined and symlink-checked. `/var/minis/workspace`
and `/var/minis/attachments` are mapped to the session directories, preserving
the app's prompts and skills. Paths under the device-local attachment, skill,
offload and browser roots are bridged back to the client instead of reading the
server workspace.

The container is attached to a dedicated `minis_net` bridge shared only with
Caddy. It deliberately does not join the VPS's `internal` Docker network, so a
prompt-injected shell command cannot reach unrelated containers.

## Storage

- `/data/config.json` — providers, auth token, update token, sandbox settings
  (mode 600).
- `/data/sessions/<id>/meta.json` + `messages.jsonl` — append-only session
  history.
- `/data/workspaces/<id>/` — persistent session workspace.
- `/data/bin/minis-server` — last good built binary.
- `/data/go-cache`, `/data/go-mod` — startup build cache.

## Efficiency

- No third-party Go modules; the binary is ~7 MB.
- Idle RSS is ~8 MB with no background goroutines or polling.
- SSE replaces polling and lets each turn finish as soon as the provider does.
- The startup build is the only compiler use; the compiled binary is cached and
  used if a later clone/build fails.
