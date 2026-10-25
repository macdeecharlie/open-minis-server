#!/bin/sh
set -eu

APP_DIR="/app"
DATA_DIR="/data"
CACHE_DIR="$DATA_DIR/app-cache"
BIN_DIR="$DATA_DIR/bin"
BIN="$BIN_DIR/minis-server"
NEW_BIN="$BIN_DIR/minis-server.new"

APP_GIT_BRANCH="${APP_GIT_BRANCH:-main}"
APP_GIT_REPO="${APP_GIT_REPO:-https://github.com/macdeecharlie/open-minis-server.git}"
APP_GIT_TOKEN="${APP_GIT_TOKEN:-}"

mkdir -p "$DATA_DIR" "$CACHE_DIR" "$BIN_DIR" "$DATA_DIR/workspaces" "$DATA_DIR/sessions"
mkdir -p "$DATA_DIR/go-mod" "$DATA_DIR/go-cache"

# The shell tools run as the unprivileged "minis" user. The bind-mounted data
# directory belongs to root when the container first starts.
if [ "$(id -u)" = "0" ]; then
    chown -R 10001:10001 "$DATA_DIR/workspaces" 2>/dev/null || true
fi

clone_repo() {
    [ -n "$APP_GIT_REPO" ] || { echo "[startup] APP_GIT_REPO not set."; return 1; }
    tmp_dir="$(mktemp -d)"
    repo_dir="$tmp_dir/repo"
    if [ -n "$APP_GIT_TOKEN" ]; then
        askpass="$tmp_dir/askpass.sh"
        printf '#!/bin/sh\necho "%s"\n' "$APP_GIT_TOKEN" > "$askpass"
        chmod +x "$askpass"
        clone_env="GIT_ASKPASS=$askpass GIT_TERMINAL_PROMPT=0"
    else
        clone_env="GIT_TERMINAL_PROMPT=0"
    fi
    echo "[startup] Cloning $APP_GIT_REPO ($APP_GIT_BRANCH)..."
    if ! env $clone_env git clone --depth 1 --branch "$APP_GIT_BRANCH" "$APP_GIT_REPO" "$repo_dir"; then
        echo "[startup] Clone failed; keeping the last good build."
        rm -rf "$tmp_dir"
        return 1
    fi
    if [ ! -d "$repo_dir/app" ]; then
        echo "[startup] Repo has no app/ folder."
        rm -rf "$tmp_dir"
        return 1
    fi
    rm -rf "$APP_DIR"
    mkdir -p "$APP_DIR"
    cp -a "$repo_dir/app/." "$APP_DIR/"
    # Keep a source snapshot for diagnosis and template parity.
    rm -rf "$CACHE_DIR"
    mkdir -p "$CACHE_DIR"
    cp -a "$APP_DIR/." "$CACHE_DIR/"
    rm -rf "$tmp_dir"
    return 0
}

build_app() {
    echo "[startup] Building server..."
    (
        cd "$APP_DIR"
        GOMODCACHE="$DATA_DIR/go-mod" \
        GOCACHE="$DATA_DIR/go-cache" \
        CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w" -o "$NEW_BIN" .
    )
    chmod +x "$NEW_BIN"
    mv "$NEW_BIN" "$BIN"
}

if clone_repo; then
    if ! build_app; then
        echo "[startup] Build failed."
        if [ -x "$BIN" ]; then
            echo "[startup] Falling back to the previous binary."
        else
            echo "[startup] No previous binary; aborting."
            exit 1
        fi
    fi
else
    if [ ! -x "$BIN" ]; then
        echo "[startup] No app source and no previous binary; aborting."
        exit 1
    fi
    echo "[startup] Using previous binary."
fi

# A missing config is a configuration error, not something to paper over. The
# binary prints a complete example and exits when it cannot load the file.
exec "$@"
