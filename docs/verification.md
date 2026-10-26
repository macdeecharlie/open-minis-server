# Verification log

This file records how the refactor was verified. Commands that touched live
credentials are described by their effect; no token values are reproduced.

## Inventory coverage

`repo-code-inventory.csv` walks the checkout including the `deps/ish` and
`deps/proot` submodules: **8,067 files / 3,460,974 lines** (Android 2,186 /
866,049; iOS reference 1,314 / 1,279,362; dependencies 4,508 / 1,298,434;
shared 21 / 8,780; project 38 / 8,349). The Android-only delivery tree is
**2,178 files / 865,695 lines**. Provenance in the delivery tree: 2,144
upstream, 16 upstream-modified, 9 refactor, 9 local runtime assets; the repo
scope has 18 modified and 10 refactor rows. `unclassified` is zero. Build
output, local SDK paths and secret customization files are excluded.
`server-code-inventory.csv`: **122 files / 21,947 lines**.

## Automated checks

```sh
cd app
gofmt -l .                      # empty
go vet ./...                    # clean
go test -count=1 ./...          # passes, repeated runs (no flakes)
go test -race -count=1 ./...    # clean after waiting for the SSE subscriber
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
- request image budget, pixel-based image token estimates and single-turn
  compaction;
- native Gemini stream parsing, usage accounting and thought-signature round trip;
- native Anthropic auxiliary `/v1/chat/completions` translation (stream and JSON);
- OpenAI `max_completion_tokens`, reasoning-effort clamping/off tiers and
  Responses `[DONE]`/incomplete/content-filter paths;
- the official tool loop detector, preflight arguments and title reminder;
- shell ANSI/CR sanitation, 50k truncation, file tail reads and next_offset;
- context offload of old results/arguments/images;
- per-run provider overrides, model temperature/top_p/headers/extra body and
  quota-header forwarding;
- idempotent retried runs and provider error classification;
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
- A live mock run posted a `read_image` tool result with an image; the
  `tool_result` event carried `image`, `image_mime` and `image_path`.
- Headless server CLI endpoints still run in the sandbox; paired runs route
  `minis-sessions-cli`, `minis-config` and `minis-model-use` to the device so
  the full official settings/session surface is available.
- `mount` inside a command shows the per-session binds for workspace,
  attachments, offloads, browser, MCP config, shared, skills and memory.
- Live global-store file API: upload to `/var/minis/skills`, list, raw read
  and delete all succeed; a namespaced shell command reads the same file.
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
- Release feed lists 1.16.23/build 61; the APK is 44,861,776 bytes with
  SHA-256 `7d324ea9783fc960497011eb9f1c3a1c7838536d9bc86323a277562ec65aa7bf`,
  matching the release `.sha256` and the feed page.
- APK signature SHA-256 is the stable
  `ab5b668386da52fdc4476a9eb86a68a10daeacecb8453301ed73c25188d706ee`
  (DN `CN=Android Debug`), versionCode 61, versionName 1.16.23.
- `minis.gtoat.top` is embedded in the APK; the bearer token is not printed.
- APKs 1.16.20/build 58 through 1.16.24/build 62 use the same stable
  certificate; 1.16.24 is the source release in CI.
- A live run with a per-run provider list (temporary OpenAI-shaped SSE server
  in the sandbox) returned `provider=runtime` and the expected text, proving
  app-local provider execution does not need server config.
- A live bare run with the configured OpenRouter model returned `OK` and
  correct usage after the provider/usage changes.

## Not verified (environment limits)

- Installing the APK over an existing install and exercising the UI on a
  physical arm64 phone (no device or KVM/arm64 emulator available here).
- Real provider OAuth flows (Claude/Copilot/Gemini/Kimi/xAI) because the server
  is configured with API-key providers.
- Backup/restore of a live phone database and SAF-mounted folders.
