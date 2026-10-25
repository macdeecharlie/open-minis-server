#!/usr/bin/env python3
"""Generate a file-level accounting of the OpenMinis Android tree.

This does not decide parity by itself; it makes every source/asset file visible
with an owner (server, device, UI, dev-only) and a parity status so a missing
file or unclassified package is obvious in review.
"""
import argparse
import csv
import os
import re
from pathlib import Path

BASE_PKG = "com/openminis/app/"

# Package-level owner. More specific overrides below.
PACKAGE_OWNER = {
    "server": ("server-bridge", "implemented"),
    "provider": ("server", "partial"),
    "agent": ("server", "partial"),
    "config": ("server", "partial"),
    "quota": ("server", "partial"),
    "tools": ("server", "partial"),
    "sandbox": ("server-container", "implemented"),
    "scheduled": ("server+device-trigger", "partial"),
    "auth": ("device-oauth+server-custody", "partial"),
    "backup": ("server-format+device-transport", "partial"),
    "mcp": ("server+device-oauth", "partial"),
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
    "usage": ("server", "partial"),
    "i18n": ("ui", "ui-only"),
    "terminal": ("device+server-terminal", "partial"),
    "shared": ("device", "device-only"),
    "util": ("device", "device-only"),
}

# Specific portable logic living under ui/ (not presentation).
UI_SERVER_LOGIC = {
    "ContextTrimPlanner.kt": "server",
    "ContextUsageHint.kt": "server",
    "ContextUsageHintUi.kt": "ui",
    "ChatFlatItems.kt": "ui",
    "ChatModels.kt": "ui",
    "TitleCandidates.kt": "server",
    "StopLateGuard.kt": "server",
    "EmptyTurnRecovery.kt": "server",
    "EmptyTurnFallback.kt": "server",
    "StopConfirmPolicy.kt": "server",
    "LargeContentGuard.kt": "ui",
    "PastedTextBuffer.kt": "ui",
    "QueuedPromptBatching.kt": "server",
    "ThinkingChunking.kt": "server",
    "ToolDurationWire.kt": "ui",
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
}

# Specific files whose role differs from their package.
FILE_OWNER = {
    "UpdateChecker.kt": ("device-updater", "implemented"),
    "UpdatePolicy.kt": ("device-updater", "implemented"),
    "UpdateStateStore.kt": ("device-updater", "implemented"),
    "UpdateAutoChecker.kt": ("device-updater", "implemented"),
    "PendingUpdateStore.kt": ("device-updater", "implemented"),
    "SoulStore.kt": ("server", "partial"),
    "ToolLoopDetector.kt": ("server", "partial"),
    "AgentLoopLimits.kt": ("server", "partial"),
    "InterruptedTailDetector.kt": ("server", "partial"),
    "MemoryRepository.kt": ("server", "partial"),
    "SkillRepository.kt": ("server+device-files", "partial"),
    "MCPRepository.kt": ("server+device-files", "partial"),
    "PRootKernel.kt": ("legacy-device-sandbox", "replaced-by-server-container"),
    "RootfsManager.kt": ("legacy-device-sandbox", "replaced-by-server-container"),
    "ShellExecutor.kt": ("legacy-device-sandbox", "replaced-by-server-container"),
    "PersistentShell.kt": ("legacy-device-sandbox", "replaced-by-server-container"),
    "FreshProcessShell.kt": ("legacy-device-sandbox", "replaced-by-server-container"),
    "ExecutionCoordinator.kt": ("legacy-device-sandbox", "replaced-by-server-container"),
    "TerminalSession.kt": ("device-terminal", "pending-remote-pty"),
    "TerminalScreen.kt": ("ui-terminal", "pending-remote-pty"),
}

EXTS = {
    ".kt": "kotlin", ".java": "java", ".cpp": "native", ".cc": "native",
    ".c": "native", ".h": "native", ".hpp": "native", ".aidl": "aidl",
    ".xml": "xml", ".json": "asset", ".txt": "asset", ".md": "asset",
    ".ftl": "asset", ".html": "asset", ".js": "asset", ".css": "asset",
    ".properties": "config", ".pro": "config", ".gradle": "config",
    ".kts": "config", ".sh": "script", ".py": "script", ".so": "binary",
    ".a": "binary", ".aar": "binary", ".apk": "binary", ".jar": "binary",
}

def owner_for(rel: str) -> tuple[str, str, str]:
    p = Path(rel)
    parts = p.parts
    name = p.name
    if name in ("MainActivity.kt", "MinisApp.kt"):
        return "device-wiring", "device-only", "application/activity wiring"
    if name == "AppBuildInfo.kt":
        return "ui", "ui-only", "build/version display"
    if "META-INF/services" in rel:
        return "device", "device-only", "ACRA service registration"
    # The inventory root is src/android; normalise to the app-relative shape
    # used by the rules below.
    if rel.startswith("app/src/main/"):
        rel = rel[len("app/"):]
        parts = Path(rel).parts
    elif rel.startswith(("app/src/debug/", "app/src/test/", "app/src/androidTest/")):
        return "dev-only", "not-applicable", "test/debug"
    elif rel.startswith(("app/src/main/", "app/schemas/", "app/libs/", "app/")):
        if name.endswith((".jks", ".aar", ".apk")):
            return "device-artifact", "device-only", "signing/build artifact"
        if name in ("build.gradle.kts", "proguard-rules.pro", "provider-customization.properties", "provider-customization.properties.example", "server-customization.properties.example"):
            return "build-system", "not-applicable", "build configuration"
        if rel.startswith("app/schemas/"):
            return "server-schema", "partial", "Room schema contract"
        return "device-assets", "device-only", "app-level file"
    elif rel.startswith(("gradle/", "build.gradle.kts", "settings.gradle.kts", "gradlew", "gradle.properties", "local.properties")):
        return "build-system", "not-applicable", "build configuration"
    if name in FILE_OWNER:
        o, s = FILE_OWNER[name]
        return o, s, "file override"
    # src/main/java/com/openminis/app/<package>/...
    if "java" in parts and "com" in parts:
        try:
            idx = parts.index("com")
            if idx + 3 < len(parts) and parts[idx + 1] == "openminis" and parts[idx + 2] == "app":
                pkg = parts[idx + 3] if idx + 3 < len(parts) else "(root)"
                if pkg == "ui" and name in UI_SERVER_LOGIC:
                    return UI_SERVER_LOGIC[name], "partial", "ui logic override"
                if pkg in PACKAGE_OWNER:
                    o, s = PACKAGE_OWNER[pkg]
                    return o, s, "package"
                return "unclassified", "unclassified", "package not in matrix"
        except ValueError:
            pass
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
    if rel.startswith("src/debug/") or rel.startswith("src/test/"):
        return "dev-only", "not-applicable", "test/debug"
    return "unclassified", "unclassified", "no rule"

def count_lines(path: Path, kind: str) -> int:
    if kind in ("binary", "asset") and path.stat().st_size > 2_000_000:
        return 0
    try:
        with path.open("rb") as f:
            return sum(1 for _ in f)
    except OSError:
        return 0

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", required=True, help="OpenMinis/src/android directory")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    root = Path(args.source)
    rows = []
    for path in sorted(root.rglob("*")):
        if not path.is_file():
            continue
        rel = path.relative_to(root).as_posix()
        if any(part in (".git", "build", ".gradle", ".cxx", ".kotlin") for part in path.parts):
            continue
        kind = EXTS.get(path.suffix.lower())
        if kind is None:
            kind = "other"
        owner, status, why = owner_for(rel)
        rows.append({
            "path": rel,
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
    from collections import Counter
    print("owners:", Counter(r["owner"] for r in rows).most_common())
    print("status:", Counter(r["status"] for r in rows).most_common())

if __name__ == "__main__":
    main()
