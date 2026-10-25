# Agent run protocol

The Android client sends its existing history on every run and renders the
server's event stream. The server is the only component that talks to a model
provider; the app holds no provider keys.

## `POST /v1/agent/runs`

Bearer authentication is required. The request body is:

```json
{
  "run_id": "run_...",
  "session_id": "the app's persisted session id",
  "model": "deepseek-v4-flash",
  "prompt_context": {
    "identity": "You are <name>, a capable AI assistant ...",
    "memory_enabled": true,
    "browser_enabled": true,
    "user_ask_enabled": true,
    "delegation_enabled": true,
    "helper": false,
    "language": "en-US",
    "timezone": "Asia/Kolkata",
    "model_use_count": 4,
    "roster": "Available sub agents...",
    "skills": "Skills:\\n<available_skills>...",
    "mcp": "Available MCP Servers...",
    "global_memory": "Global memory (GLOBAL.md)...",
    "daily_memory": "Recent memories..."
  },
  "messages": [
    {
      "id": "local DB row id (optional but needed for compaction reuse)",
      "role": "user",
      "parts": [{"type": "text", "text": "hello"}]
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "shell_execute",
        "description": "...",
        "parameters": {"type": "object", "properties": {}}
      }
    }
  ],
  "max_turns": 500,
  "max_tokens": 8192,
  "thinking": "OFF",
  "fallbacks": ["another-configured-model"],
  "fallback_strategy": "default",
  "user_message_id": "the app's local DB row id"
}
```

When `prompt_context` is present the server assembles the complete system
prompt (identity/SOUL, tool bullets, switches, skills/MCP/memory fragments,
runtime suffix) and ignores `system`. A legacy client may still send `system`;
the server preserves it and appends the execution-environment note. Requests
with neither get the full server prompt (used by headless/API clients).

Part types:

| Type | Fields |
|---|---|
| `text` | `text` |
| `image` | `mime`, `data` (base64) |
| `tool_use` | `id`, `name`, `args` (JSON object), `thought_signature` (Gemini) |
| `tool_result` | `id`, `name`, `content`, `is_error` |

## Response stream

The response is `text/event-stream`. Every event carries a monotonically
increasing `id:` so a reconnect can resume exactly:

```
id: 7
event: text
data: {"text":"Hello"}
```

| Event | Data |
|---|---|
| `ready` | `run_id`, `session_id`, `model`, `provider` |
| `text` | `text` (delta) |
| `thinking` | `text` (delta) |
| `tool_start` | `id`, `name`, `executor` (`server` or `client`) |
| `tool_input` | `id`, `delta` (raw JSON fragment) |
| `tool_call` | `id`, `name`, `args`, `executor` |
| `tool_running` | `id`, `name` (server tool started) |
| `tool_output` | `id`, `line` (live shell output) |
| `await_client` | `id`, `name` (device tool waiting for the app) |
| `tool_result` | `id`, `name`, `content`, `is_error`, `title`, `duration_ms`, `executor`, `image` (base64), `image_mime`, `image_path` |
| `assistant_end` | `parts`, `stop_reason`, `usage`, `reasoning_content` |
| `tools_end` | `results` |
| `usage` | token usage object |
| `compacted` | `summary`, `messages`, `tokens`, `window`, `compaction` |
| `steered` | `client_id`, `text` |
| `steered` | `client_id`, `text` |
| `info` | `text` (visible notice: fallback, truncated images, loop warning) |
| `truncated` | `skipped` (only if a replay window was exceeded) |
| `done` | `stop_reason`, `turns` |
| `error` | `message` |

Image-bearing tool results (browser screenshots, `read_image`) are carried as
base64 `image` + `image_mime`; the client persists them on the matching
`AgentContentPart.ToolResult`. OpenAI-compatible providers receive them as a
following user image carrier, Anthropic inside the `tool_result` block, and
Gemini as `inlineData`.

## Reconnect and replay

A dropped connection does **not** cancel the run. Reconnect with:

```http
GET /v1/agent/runs/{run_id}/events?after=7
Authorization: Bearer ...
Accept: text/event-stream
```

`Last-Event-ID` is also honored. Completed runs remain replayable for the last
20 runs (count-bounded, no timer). If the run has been evicted, the endpoint
returns 404; load the stored session instead.

The app caches device-tool results by `id`: a replayed `await_client` repeats
the POST if needed but never repeats the device side effect.

## Device-bound tools and CLIs

The server executes `shell_execute`, `file_read`, `file_write`, `file_edit`,
`web_fetch` and (when configured) `web_search`. Every other tool in the request
schema is offered to the model but executed by the Android app. The server
emits `await_client` and pauses that run until the app posts:

```http
POST /v1/agent/runs/{run_id}/tool_result
Authorization: Bearer ...
Content-Type: application/json

{"id":"call_...","name":"browser_use","content":"...","is_error":false,"title":"...","duration_ms":123}
```

A `shell_execute` command that invokes an `android-*`, `minis-open` or
`minis-scheduled` CLI, or that references a device-only path
(`/var/minis/{attachments,skills,memory,shared,mounts,browser}`), is routed to
the app the same way. The app runs it in its existing device shell, exactly like
the official build.

`minis-sessions-cli`, `minis-config`, `minis-model-use` and `minis-mcp-cli` run
inside the Debian sandbox; they call `/v1/cli/*` with the sandbox token or read
the shared MCP config. See the parity matrix for the currently supported
subcommands.

## Steering

```http
POST /v1/agent/runs/{run_id}/steer
Authorization: Bearer ...
Content-Type: application/json

{"text":"also check the logs","client_id":"queued_..."}
```

The server appends the message and drains pending steers at the start of the
next provider turn. A steer that arrives after the run finished is not applied;
the client should keep its queued prompt for the next run.

## Cancellation

`POST /v1/agent/runs/{id}/cancel` cancels the run and kills its shell process
group. Closing the SSE connection alone does not cancel; the client calls this
endpoint when the user taps Stop.

## Session persistence

The server stores the user prompt named by `user_message_id` (idempotent),
each assistant turn, each tool-result batch, a title, and the newest compaction
summary anchored to a message id. The client remains the source of truth for
the request history; the stored copy exists for the future Web UI, the
`minis-sessions-cli`, and diagnostics.
