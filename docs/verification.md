# Verification log

This file records how the refactor was verified. Commands that touched live
credentials are described by their effect; no token values are reproduced.

## Automated checks

```sh
cd app
gofmt -l .                      # empty
go vet ./...                    # clean
go test -count=1 ./...          # passes, repeated runs (no flakes)
go build -o /tmp/minis-server .
```

The test suite covers:

- SSE agent loop and event order;
- client-tool `await_client` round trip;
- device CLI shell routing (`android-device`, `minis-*`);
- server shell/file tools (read/write/edit/list/raw/upload/delete/rename/mkdir);
- session create/message persistence/title/delete/workspace removal;
- scheduled create/list/run/delete and headless execution;
- remote sandbox round trip, PTY terminal, session removal;
- model retry and fallback;
- prompt assembly (skills, memory on/off, browser/user-ask switches, runtime);
- context estimator, compaction split/pairing/summary reuse, offload;
- request image budget;
- native Gemini stream parsing and thought-signature round trip;
- native Anthropic auxiliary `/v1/chat/completions` translation (stream and JSON);
- OpenAI `max_completion_tokens` / reasoning-effort clamping;
- CLI endpoints and sandbox-token isolation from the app token;
- `minis-config` pending-confirmation and one-shot token flow;
- run replay (mid-run and after completion) and steering;
- global MCP config shared across sessions.

## Live VPS checks

- `https://minis.gtoat.top/healthz` returns `ok` after every deployment.
- Debian sandbox: `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"`, `uid=0(root)`,
  `apt-get`, Node, Python, gcc, `proot` absent; PTY command execution verified.
- Real OpenRouter run with the server-owned prompt returned Debian
  `uname`/`os-release` and confirmed the injected skills fragment.
- Real OpenRouter run created a Python script that opened
  `/var/minis/workspace/input.txt` by absolute path inside the script; the
  script printed the expected content. `pwd` is `/var/minis/workspace`, and
  `/workspace` exposes only the current session (other sessions are hidden).
- `mount` inside a command shows the per-session binds for workspace,
  attachments, offloads, browser and MCP config.
- A second real run asked for `android-device info` and the server emitted
  `await_client`; posting a simulated device result continued the same run.
- Server file API: upload → mkdir → rename → raw read → delete → session delete;
  the workspace disappeared from the sandbox.
- CLI inside the sandbox:
  - `minis-sessions-cli list/messages`
  - `minis-config get defaults`, `set default_model ...` → pending token →
    `confirm` → applied and persisted (mode 600), reused token rejected.
  - `minis-model-use list` and a real `run` returning a model response.
  - `minis-scheduled list`.
  - `minis-mcp-cli list/tools/call` against a temporary stdio MCP server,
    returning the expected tool output.
- Release feed lists 1.16.6/build 44; authenticated APK download is 44,848,236
  bytes and its digest matches the release `.sha256`.
- APK signature SHA-256 is the stable
  `ab5b668386da52fdc4476a9eb86a68a10daeacecb8453301ed73c25188d706ee`
  (DN `CN=Android Debug`), versionCode 44, versionName 1.16.6.
- `minis.gtoat.top` is embedded in the APK; the bearer token is not printed.
- APK 1.16.7/build 45 has the same stable certificate and a matching digest.

## Not verified (environment limits)

- Installing the APK over an existing install and exercising the UI on a
  physical arm64 phone (no device or KVM/arm64 emulator available here).
- Real provider OAuth flows (Claude/Copilot/Gemini/Kimi/xAI) because the server
  is configured with API-key providers.
- Backup/restore of a live phone database and SAF-mounted folders.
