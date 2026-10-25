#!/usr/bin/env python3
"""Generate a line-level accounting of the OpenMinis repository.

Two modes:

* ``--mode android`` (default): every file under ``src/android`` with an owner
  (server, device, UI, dev-only) and a parity status.
* ``--mode repo``: every tracked file in the whole repository, with a
  ``scope`` column. Android is the delivery target; iOS is the behavioural
  reference; dependencies, scripts and docs are accounted for explicitly
  rather than silently omitted.

The inventory does not prove parity by itself. It makes every file visible and
gives each one a classification, so a new or missing file is obvious in review.
"""
import argparse
import csv
import subprocess
from collections import Counter
from pathlib import Path

# Package-level owner for Android source, with file overrides below.
PACKAGE_OWNER = {
    "server": ("server-bridge", "implemented"),
    "provider": ("server", "partial"),
    "agent": ("server", "partial"),
    "config": ("server", "partial"),
    "quota": ("server", "partial"),
    "tools": ("server", "partial"),
    "sandbox": ("device-local-fallback", "local-fallback"),
    "scheduled": ("device-trigger+server-runner", "implemented"),
    "auth": ("device-oauth+server-custody", "partial"),
    "backup": ("device", "device-only"),
    "mcp": ("device+server-prompt", "partial"),
    "data": ("server+device", "partial"),
    "ui": ("ui", "ui-only"),
    "browser": ("device", "device-only"),
    "speech": ("device+server-llm", "device-only"),
    "service": ("device", "device-only"),
    "notification": ("device", "device-only"),
    "network": ("device", "device-only"),
    "share": ("device", "device-only"),
    "webapp": ("device", "device-only"),
    "accessibility": ("device", "device-only"),
    "offload": ("device", "device-only"),
    "power": ("device", "device-only"),
    "deeplink": ("device", "device-only"),
    "providers": ("device", "device-only"),
    "debug": ("dev-only", "not-applicable"),
    "crash": ("device", "device-only"),
    "logging": ("device", "device-only"),
    "diagnostics": ("device", "device-only"),
    "usage": ("server+device", "partial"),
    "i18n": ("ui", "ui-only"),
    "terminal": ("device+server-terminal", "implemented"),
    "shared": ("device", "device-only"),
    "util": ("device", "device-only"),
}

# Portable agent logic living under ui/ (not presentation).
UI_SERVER_LOGIC = {
    "ContextTrimPlanner.kt": "server",
    "ContextUsageHint.kt": "server",
    "TitleCandidates.kt": "server",
    "StopLateGuard.kt": "server",
    "EmptyTurnRecovery.kt": "server",
    "EmptyTurnFallback.kt": "server",
    "StopConfirmPolicy.kt": "server",
    "QueuedPromptBatching.kt": "server",
    "ThinkingChunking.kt": "server",
    "ProviderAvailability.kt": "server",
    "TitlePromptLocale.kt": "server",
    "SessionTitleRules.kt": "server",
    "SessionTitleRegenerator.kt": "server",
    "ChatViewModel.kt": "server+ui-state",
    "ChatViewModelSlashExt.kt": "ui",
    "ChatViewModelMentionExt.kt": "ui",
    "ChatViewModelUiStateExt.kt": "ui",
    "ChatViewModelStore.kt": "ui",
    "ChatLinkResolver.kt": "ui",
    "ToolFoldPlanner.kt": "ui",
    "ChatSearchEngine.kt": "ui",
    "ContextUsageHintUi.kt": "ui",
    "ChatFlatItems.kt": "ui",
    "ChatModels.kt": "ui",
    "LargeContentGuard.kt": "ui",
    "PastedTextBuffer.kt": "ui",
    "ToolDurationWire.kt": "ui",
}

# Files whose role differs from their package.
FILE_OWNER = {
    "UpdateChecker.kt": ("device-updater", "implemented"),
    "UpdatePolicy.kt": ("device-updater", "implemented"),
    "UpdateStateStore.kt": ("device-updater", "implemented"),
    "UpdateAutoChecker.kt": ("device-updater", "implemented"),
    "PendingUpdateStore.kt": ("device-updater", "implemented"),
    "SoulStore.kt": ("server", "partial"),
    "ToolLoopDetector.kt": ("server", "partial"),
    "AgentLoopLimits.kt": ("server", "implemented"),
    "InterruptedTailDetector.kt": ("server", "partial"),
    "MemoryRepository.kt": ("server", "partial"),
    "SkillRepository.kt": ("server+device-files", "partial"),
    "MCPRepository.kt": ("server+device-files", "partial"),
    "PRootKernel.kt": ("legacy-device-sandbox", "local-fallback"),
    "RootfsManager.kt": ("legacy-device-sandbox", "local-fallback"),
    "ShellExecutor.kt": ("legacy-device-sandbox", "local-fallback"),
    "PersistentShell.kt": ("legacy-device-sandbox", "local-fallback"),
    "FreshProcessShell.kt": ("legacy-device-sandbox", "local-fallback"),
    "ExecutionCoordinator.kt": ("legacy-device-sandbox", "local-fallback"),
    "TerminalSession.kt": ("device-terminal", "local-fallback"),
    "TerminalSessionLike.kt": ("device+server-terminal", "implemented"),
    "TerminalScreen.kt": ("ui-terminal", "implemented"),
    "RemoteTerminalSession.kt": ("server-bridge", "implemented"),
    "ServerFileSync.kt": ("server-bridge", "implemented"),
    "ServerAgentClient.kt": ("server-bridge", "implemented"),
    "ServerSettings.kt": ("server-bridge", "implemented"),
    "ServerBootstrap.kt": ("server-bridge", "implemented"),
    "ServerWire.kt": ("server-bridge", "implemented"),
}

# PRoot/shell engine files kept only for offline/local mode. The paired server
# is the execution environment; these are not the agent's default path.
LOCAL_FALLBACK_SANDKBOX = {
    "PRootKernel.kt", "RootfsManager.kt", "ShellExecutor.kt",
    "PersistentShell.kt", "FreshProcessShell.kt", "ExecutionCoordinator.kt",
    "ShellExecutionStrategy.kt", "GlobalShellThrottle.kt",
}

EXTS = {
    ".kt": "kotlin", ".java": "java", ".cpp": "native", ".cc": "native",
    ".c": "native", ".h": "native", ".hpp": "native", ".aidl": "aidl",
    ".xml": "xml", ".json": "asset", ".txt": "asset", ".md": "asset",
    ".ftl": "asset", ".html": "asset", ".js": "asset", ".css": "asset",
    ".properties": "config", ".pro": "config", ".gradle": "config",
    ".kts": "config", ".sh": "script", ".py": "script", ".so": "binary",
    ".a": "binary", ".aar": "binary", ".apk": "binary", ".jar": "binary",
    ".swift": "swift", ".m": "objective-c", ".mm": "objective-c",
    ".cxx": "native", ".yml": "yaml", ".yaml": "yaml", ".toml": "config",
    ".rs": "rust", ".go": "go", ".plist": "plist", ".entitlements": "plist",
}


def classify_android(rel: str) -> tuple[str, str, str]:
    """Classify a path relative to ``src/android``."""
    p = Path(rel)
    parts = p.parts
    name = p.name
    if name in ("MainActivity.kt", "MinisApp.kt"):
        return "device-wiring", "device-only", "application/activity wiring"
    if name == "AppBuildInfo.kt":
        return "ui", "ui-only", "build/version display"
    if "META-INF/services" in rel:
        return "device", "device-only", "ACRA service registration"

    if rel.startswith("app/src/debug/") or rel.startswith("app/src/test/") or rel.startswith("app/src/androidTest/"):
        return "dev-only", "not-applicable", "test/debug"
    if rel.startswith(("app/src/main/", "app/schemas/", "app/libs/", "app/")):
        if name.endswith((".jks", ".aar", ".apk")):
            return "device-artifact", "device-only", "signing/build artifact"
        if name in (
            "build.gradle.kts", "proguard-rules.pro",
            "provider-customization.properties",
            "provider-customization.properties.example",
            "server-customization.properties.example",
        ):
            return "build-system", "not-applicable", "build configuration"
        if rel.startswith("app/schemas/"):
            return "server-schema", "partial", "Room schema contract"
        if name in FILE_OWNER:
            owner, status = FILE_OWNER[name]
            return owner, status, "file override"
        # Source paths are normalised to app-relative form below.
        tail = rel.split("app/", 1)[1]
        rel = tail
        parts = Path(tail).parts
    elif rel.startswith(("gradle/", "build.gradle.kts", "settings.gradle.kts", "gradlew", "gradle.properties", "local.properties")):
        return "build-system", "not-applicable", "build configuration"
    else:
        if name in FILE_OWNER:
            owner, status = FILE_OWNER[name]
            return owner, status, "file override"

    if "java" in parts and "com" in parts:
        idx = parts.index("com")
        if idx + 3 <= len(parts) and parts[idx + 1] == "openminis" and parts[idx + 2] == "app":
            pkg = parts[idx + 3] if idx + 3 < len(parts) else "(root)"
            sub = parts[idx + 4] if idx + 4 < len(parts) else ""
            if pkg == "ui" and name in UI_SERVER_LOGIC:
                return UI_SERVER_LOGIC[name], "ui-only" if UI_SERVER_LOGIC[name] == "ui" else "partial", "ui logic override"
            if pkg == "sandbox" and sub == "offload":
                return "device", "device-only", "Android native_offload CLI handler"
            if pkg == "sandbox" and name in LOCAL_FALLBACK_SANDKBOX:
                return "legacy-device-sandbox", "local-fallback", "PRoot shell, offline/local mode only"
            if name in FILE_OWNER:
                owner, status = FILE_OWNER[name]
                return owner, status, "file override"
            if pkg in PACKAGE_OWNER:
                owner, status = PACKAGE_OWNER[pkg]
                return owner, status, "package"
            return "unclassified", "unclassified", "package not in matrix"
    if rel.startswith("src/main/cpp/"):
        return "device-native", "device-only", "JNI/native helper"
    if rel.startswith("src/main/assets/"):
        if "proot" in name or "alpine-minirootfs" in name or "alpine" in name:
            return "legacy-device-sandbox", "replaced-by-server-container", "legacy PRoot/rootfs asset"
        return "device-assets", "device-only", "bundled runtime/UI asset"
    if rel.startswith("src/main/res/"):
        return "ui-resources", "ui-only", "Android resources"
    if rel.startswith("src/main/AndroidManifest.xml"):
        return "device-manifest", "device-only", "Android manifest"
    if rel.startswith("src/main/jniLibs/"):
        return "device-native", "device-only", "native library"
    return "unclassified", "unclassified", "no rule"


def classify_repo(rel: str) -> tuple[str, str, str, str]:
    """Return scope, owner, status, reason for a repository-relative path."""
    if rel.startswith("src/android/"):
        owner, status, why = classify_android(rel[len("src/android/"):])
        return "android", owner, status, why
    if rel.startswith("src/ios/"):
        return "ios-reference", "ios-reference", "reference", "iOS is the behavioural reference; Android is the delivery target"
    if rel.startswith("src/shared/"):
        return "shared", "shared-resource", "device-only", "cross-platform assets shared by the apps"
    if rel.startswith("deps/"):
        return "dependency", "third-party-dependency", "not-applicable", "vendored upstream source, not OpenMinis logic"
    if rel.startswith(("scripts/", "docs/", ".github/", "assets/")):
        return "project", "project", "not-applicable", "build, docs, CI or repository tooling"
    if "/" not in rel:
        return "project", "project", "not-applicable", "repository metadata"
    return "unclassified", "unclassified", "unclassified", "no rule"


def count_lines(path: Path, kind: str) -> int:
    # Huge generated/binary assets are counted as 0 lines rather than
    # pretending a tarball has textual lines.
    if kind in ("binary", "asset") and path.stat().st_size > 2_000_000:
        return 0
    try:
        with path.open("rb") as f:
            return sum(1 for _ in f)
    except OSError:
        return 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", required=True, help="repository root or src/android")
    ap.add_argument("--out", required=True)
    ap.add_argument("--mode", choices=("android", "repo"), default="android")
    args = ap.parse_args()
    root = Path(args.source)
    android_root = root if (root / "app/src/main").exists() else root / "src/android"
    use_git = args.mode == "repo"

    files: list[str] = []
    if use_git and (root / ".git").exists():
        out = subprocess.check_output(
            ["git", "-C", str(root), "ls-files", "--cached", "--others", "--exclude-standard"],
            text=True,
        )
        files = [line for line in out.splitlines() if line.strip()]
    else:
        base = root if args.mode == "android" else root
        for path in sorted(base.rglob("*")):
            if not path.is_file():
                continue
            if any(part in (".git", "build", ".gradle", ".cxx", ".kotlin", "node_modules") for part in path.parts):
                continue
            rel = path.relative_to(base).as_posix()
            if args.mode == "android" and not rel.startswith("app/"):
                continue
            files.append(rel)

    rows = []
    for rel in files:
        if args.mode == "android":
            path = android_root / rel
            if not path.is_file():
                continue
            owner, status, why = classify_android(rel)
            rows.append({
                "path": rel,
                "scope": "android",
                "kind": EXTS.get(path.suffix.lower(), "other"),
                "lines": count_lines(path, EXTS.get(path.suffix.lower(), "other")),
                "bytes": path.stat().st_size,
                "owner": owner,
                "status": status,
                "classification_reason": why,
            })
            continue
        path = root / rel
        if not path.is_file():
            continue
        scope, owner, status, why = classify_repo(rel)
        kind = EXTS.get(path.suffix.lower(), "other")
        rows.append({
            "path": rel,
            "scope": scope,
            "kind": kind,
            "lines": count_lines(path, kind),
            "bytes": path.stat().st_size,
            "owner": owner,
            "status": status,
            "classification_reason": why,
        })

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()))
        w.writeheader()
        w.writerows(rows)

    print(f"wrote {len(rows)} files to {out}")
    print("scope:", Counter(r["scope"] for r in rows).most_common())
    print("owners:", Counter(r["owner"] for r in rows).most_common())
    print("status:", Counter(r["status"] for r in rows).most_common())
    total = sum(r["lines"] for r in rows)
    print("lines:", total)
    print("lines by scope:", {k: sum(r["lines"] for r in rows if r["scope"] == k) for k in sorted({r["scope"] for r in rows})})
    un = [r["path"] for r in rows if r["status"] == "unclassified"]
    if un:
        print("UNCLASSIFIED:", un[:20])


if __name__ == "__main__":
    main()
