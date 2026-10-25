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
  "system": "system prompt built by the client",
  "messages": [
    {
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
  "max_turns": 12,
  "max_tokens": 8192,
  "thinking": "OFF",
  "user_message_id": "the app's local DB row id"
}
```

Part types:

| Type | Fields |
|---|---|
| `text` | `text` |
| `image` | `mime`, `data` (base64) |
| `tool_use` | `id`, `name`, `args` (JSON object) |
| `tool_result` | `id`, `name`, `content`, `is_error` |

The response is `text/event-stream`. Events are ordered:

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
| `tool_result` | `id`, `name`, `content`, `is_error`, `title`, `duration_ms`, `executor` |
| `assistant_end` | `parts`, `stop_reason`, `usage` |
| `tools_end` | `results` |
| `usage` | token usage object |
| `done` | `stop_reason`, `turns` |
| `error` | `message` |

`assistant_end` closes one model turn. The client persists that turn, then
waits for `tools_end` if tools ran. `done` closes the run.

### Device-bound tools

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

The same SSE response then continues. This keeps browser automation, memory,
sub-agents, user questions and device integrations on the device while the
agent loop and all model access stay on the server.

## Cancellation

Closing the SSE connection cancels the run and kills its process group.
`POST /v1/agent/runs/{id}/cancel` does the same explicitly.

## Session persistence

The server stores the user prompt named by `user_message_id` (idempotent),
each assistant turn, and each tool-result batch. The client remains the source
of truth for the request history; the stored copy exists for the future Web UI
and for diagnostics.
