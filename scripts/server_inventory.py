#!/usr/bin/env python3
"""Generate a line-level inventory of the refactor's own server/deployment code."""
import csv
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
EXCLUDE_DIRS = {".git", "data", "__pycache__"}
EXCLUDE_FILES = {"docs/server-code-inventory.csv", "docs/android-code-inventory.csv", "docs/repo-code-inventory.csv"}

ROLES = {
    "agent.go": "agent loop, retries, fallback, tool dispatch, replay/steer",
    "context.go": "context estimator, compaction, offload, image budget, tool-pair repair",
    "prompt.go": "server-owned prompt assembly",
    "provider.go": "provider HTTP, SSE, idle watchdog, error classification",
    "provider_openai.go": "OpenAI-compatible wire",
    "provider_anthropic.go": "Anthropic wire, prompt cache",
    "provider_gemini.go": "Gemini wire, thought signatures, audio",
    "provider_mock.go": "offline test provider",
    "server.go": "HTTP routes, SSE, sessions, files, terminal proxy, config",
    "store.go": "append-only session store",
    "scheduled.go": "server scheduler and headless runner",
    "sandbox.go": "local/agent sandbox, mount namespace, file tools",
    "sandbox_agent.go": "sandbox container API",
    "sandbox_remote.go": "server to sandbox client",
    "sandbox_terminal.go": "PTY terminal",
    "tools.go": "tool schemas, routing, execution, minis URLs",
    "config.go": "configuration and validation",
    "admin.go": "admin config endpoints",
    "api_cli.go": "minis-* CLI API",
    "api_files.go": "file API",
    "api_openai.go": "auxiliary OpenAI-compatible proxy/translation",
    "api_scheduled.go": "scheduled task API",
    "api_terminal.go": "terminal API proxy",
    "cli.go": "minis-* CLI implementations",
    "update.go": "private release proxy",
    "main.go": "entrypoint and role dispatch",
    "types.go": "wire/config/session types",
    "app_test.go": "test suite",
}


def role(path: Path) -> str:
    rel = path.relative_to(ROOT).as_posix()
    if path.name in ROLES:
        return ROLES[path.name]
    if path.suffix == ".txt" and str(path).startswith("app/prompt"):
        return "prompt asset"
    if rel.startswith("docker/sandbox/mcp-cli/"):
        return "bundled MCP client (from the official assets)"
    if rel.startswith("docker/"):
        return "sandbox image and deployment"
    if rel.startswith(".github/workflows/"):
        return "CI"
    if rel.startswith("scripts/"):
        return "inventory tooling"
    if rel.startswith("docs/"):
        return "documentation"
    if rel.startswith("app/vendor/"):
        return "vendored third-party (creack/pty)"
    return "server/deployment file"


def include(path: Path) -> bool:
    rel = path.relative_to(ROOT).as_posix()
    if any(part in EXCLUDE_DIRS for part in path.parts):
        return False
    if rel in EXCLUDE_FILES:
        return False
    if rel.startswith("app/vendor/github.com/creack/pty/"):
        return True
    if rel.startswith(("app/", "docker/", "scripts/", "docs/", ".github/")):
        return True
    return rel in {"README.md", ".gitignore", "config.example.json"}


def main():
    rows = []
    for path in sorted(ROOT.rglob("*")):
        if not path.is_file() or not include(path):
            continue
        try:
            lines = sum(1 for _ in path.open("rb"))
        except OSError:
            lines = 0
        rel = path.relative_to(ROOT).as_posix()
        rows.append({"path": rel, "lines": lines, "role": role(path)})
    out = ROOT / "docs/server-code-inventory.csv"
    with out.open("w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=["path", "lines", "role"])
        w.writeheader()
        w.writerows(rows)
    print(f"wrote {len(rows)} files / {sum(r['lines'] for r in rows)} lines to {out}")


if __name__ == "__main__":
    main()
