# open-minis-server

The backend for OpenMinis. The Android app is the GUI; this server owns the
agent loop, model credentials, provider streaming, shell/file tools, sessions
and the app update channel.

- **Go, one vendored dependency** — `creack/pty` for the real terminal. No
  database driver, no framework, no background daemons.
- **SSE streaming** — one long-lived HTTP response per agent run, no polling.
- **Session store** — append-only JSONL per session under `/data/sessions`.
  Startup is an in-memory index build; the server does not hold chat history in
  memory after a request finishes.
- **Dedicated full-Linux sandbox** — shell and file tools run as root inside a
  separate Debian 12 container, with per-session workspaces, path confinement,
  output caps and command timeouts. Device-bound tools (browser, sensors,
  notifications, `android-*` CLIs) are bridged back to the Android client over
  the same run; `minis-sessions-cli`, `minis-config` and `minis-model-use` run
  in the sandbox against server APIs.
- **Caddy-ready** — plain HTTP on `:8080`; TLS and the public hostname belong to
  the existing Caddy container.

## Layout

```text
app/                 Go server source (the entrypoint builds this at startup)
docker/              Dockerfile, entrypoint and a compose example
docs/                API and architecture notes
config.example.json  Complete configuration example
```

## Run locally

```sh
cd app
MINIS_CONFIG=/tmp/minis-config.json go run .
```

A local config can use the offline mock provider:

```json
{
  "public_url": "http://127.0.0.1:8080",
  "auth_token": "dev-token",
  "default_model": "mock-1",
  "providers": [
    {
      "id": "mock",
      "kind": "mock",
      "models": [{"id": "mock-1", "name": "Mock", "context_window": 128000}]
    }
  ]
}
```

```sh
curl -H 'Authorization: Bearer dev-token' http://127.0.0.1:8080/v1/models
```

## Deploy on the VPS

The image follows the standard VPS template: the image holds the Go toolchain
and the startup script, and the server source is cloned from GitHub on every
container start. A restart therefore deploys the latest `main` branch.

```sh
sudo mkdir -p /opt/open-minis-server
sudo git clone https://github.com/macdeecharlie/open-minis-server.git /opt/open-minis-server
sudo mkdir -p /opt/open-minis-server/data
# place config.json in /opt/open-minis-server/data/config.json (mode 600)

cd /opt/open-minis-server/docker
sudo docker compose up -d --build
```

The compose example runs three containers:

- `open-minis-server` — agent loop, model credentials, sessions and update proxy.
- `minis-sandbox` — a dedicated **full Debian** execution environment. The
  agent's shell, file tools and workspace live here; it has no model or update
  credentials, no Docker socket and no host mounts beyond its workspace.
- `minis-proxy` — a one-target `socat` forwarder between Caddy and the server.

The server is on `minis_net` + `sandbox_net`; the sandbox is only on
`sandbox_net`; the forwarder is on `minis_net` + `internal`; Caddy is only on
`internal`. Create both networks first:

```sh
sudo docker network create minis_net
sudo docker network create --subnet 172.21.0.0/16 minis_sandbox_net
sudo mkdir -p /opt/open-minis-server/data/sandbox
sudo chmod 755 /opt/open-minis-server/data/sandbox
```

Put a long random value in `/opt/open-minis-server/.env`:

```sh
MINIS_SANDBOX_TOKEN=...
```

Add this to the Caddyfile:

```caddy
minis.gtoat.top {
    reverse_proxy minis-proxy:8080
}
```

Then validate and reload Caddy:

```sh
sudo docker exec caddy caddy validate --config /etc/caddy/Caddyfile
sudo docker exec caddy caddy reload --config /etc/caddy/Caddyfile
```

## Configuration

Copy `config.example.json` to `/data/config.json` and set:

| Field | Meaning |
|---|---|
| `public_url` | Public HTTPS origin; used for update asset URLs. |
| `auth_token` | Bearer token the Android app sends. Long random value. |
| `default_model` | Model used when a request does not name one. |
| `providers[]` | `kind` is `openai`, `openai-responses`, `anthropic`, `gemini` or `mock`. |
| `providers[].api_key_env` | Read the key from this environment variable instead of storing it in JSON. |
| `update.repo` | Private Android repository (`owner/repo`) holding APK releases. |
| `update.token_env` | Environment variable holding a GitHub token with read access to that repo. |
| `searxng_url` | Optional. Enables the `web_search` tool. |
| `sandbox.work_root` | Persistent workspace root, default `/data/workspaces`. |

The server requires an auth token and at least one configured model. It fails
at startup rather than starting with a silent fallback.

## API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/healthz` | Public liveness/version check. |
| `GET` | `/v1/models` | OpenAI-shaped model catalog for the app's model picker. |
| `POST` | `/v1/chat/completions` | Credential-hiding proxy for auxiliary app calls (titles, compaction). |
| `POST` | `/v1/agent/runs` | Start a streamed agent run (SSE). |
| `POST` | `/v1/agent/runs/{id}/tool_result` | Return a device-bound tool result. |
| `POST` | `/v1/agent/runs/{id}/cancel` | Stop a run and its shell processes. |
| `GET` | `/v1/agent/runs/{id}/events` | Replay a run from `?after=N` / Last-Event-ID. |
| `POST` | `/v1/agent/runs/{id}/steer` | Inject a user message between turns. |
| `GET` | `/v1/sessions` | Session index for the future Web UI. |
| `GET` | `/v1/sessions/{id}` | Stored messages for one session. |
| `GET/POST` | `/v1/scheduled` | List or upsert server-owned scheduled tasks. |
| `DELETE` | `/v1/scheduled/{id}` | Delete a scheduled task. |
| `POST` | `/v1/scheduled/{id}/run` | Run a scheduled task now. |
| `POST` | `/v1/files/list` | List a session workspace directory. |
| `GET` | `/v1/files/raw` | Raw file bytes for preview/download. |
| `POST` | `/v1/files/upload` | Upload bytes into a session workspace. |
| `POST` | `/v1/files/delete` | Delete a workspace file or directory. |
| `POST` | `/v1/files/rename` | Rename or move a workspace entry. |
| `POST` | `/v1/files/mkdir` | Create a workspace directory. |
| `POST` | `/v1/terminal` | Open a PTY in the Debian sandbox (SSE). |
| `GET` | `/v1/terminal/{id}` | Reattach a live PTY from `?after=N` bytes. |
| `POST` | `/v1/terminal/{id}/input` | Send terminal input bytes. |
| `POST` | `/v1/terminal/{id}/resize` | Resize the PTY. |
| `POST` | `/v1/cli/config/confirm` | Apply a user-approved `minis-config` change. |
| `POST` | `/v1/terminal/{id}/close` | Close the terminal. |
| `POST` | `/v1/sessions/{id}/meta` | Update a server session title. |
| `GET/POST` | `/v1/cli/*` | Sandbox-token API used by the `minis-*` CLI tools. |
| `DELETE` | `/v1/sessions/{id}` | Delete a session. |
| `GET` | `/v1/app/releases` | Private GitHub release proxy for the in-app updater. |
| `GET` | `/v1/app/assets/{id}` | Authenticated APK download proxy. |
| `POST` | `/v1/admin/provider` | Upsert a provider and restart with it. Authenticated. |
| `POST` | `/v1/admin/update-token` | Store the private-release GitHub token. Authenticated. |
| `POST` | `/v1/admin/config` | Replace the whole config and restart. Authenticated. |

The full run protocol is in [`docs/agent-protocol.md`](docs/agent-protocol.md).

## Update flow

The Android updater reads `/v1/app/releases`, which the server fetches from the
private `update.repo` with a server-side token. Asset URLs are rewritten to the
server's authenticated proxy. The GitHub token never reaches the APK, and the
app never needs to know the private repository exists.

The admin endpoints exist so a token or provider key can be provisioned without
putting it in a shell command or image. Each writes `/data/config.json`
(mode 600) and exits; the container restart policy applies it. Use the same
bearer token as every other API call:

```sh
curl -X POST https://minis.example.com/v1/admin/update-token \
  -H "Authorization: Bearer $MINIS_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"token":"github_pat_..."}'
```

## Security notes

- The server is single-token by design; use a long random token and keep it in
  the app build's private properties file.
- Shell commands run as **root inside the dedicated sandbox container**, not on
  the VPS. Workspaces are isolated per session, but sessions share that root
  user and filesystem; this is a personal server, not a multi-tenant service.
  The sandbox holds no model, update or app tokens; the server holds those.
- The sandbox can reach the server on `:8080` for the `minis-*` CLI tools. That
  token is already readable by the sandbox root shell and only opens the
  `/v1/cli/*` and sandbox tool APIs, not the app's `/v1/agent` surface.
- The sandbox runs with `SYS_ADMIN` and unconfined AppArmor so each command can
  create a private mount namespace and bind the session's real directories at
  `/var/minis/...`. It has no host filesystem mounts, no Docker socket and no
  model/update/app credentials. The capability is container-local; it is what
  lets unmodified scripts resolve the same paths as the official app.
- `web_fetch` refuses loopback, private, link-local and unspecified addresses,
  and re-checks every redirect.
- The agent container is isolated from the VPS's other Docker networks and
  host listeners. `docker/egress-firewall.sh` + the systemd unit install the
  `DOCKER-USER` rules; rerun `systemctl restart minis-egress-firewall` after a
  Docker daemon restart.
- `POST /v1/agent/runs` accepts up to 64 MiB (inline images and history).
- No request bodies, tokens or provider keys are written to logs.
