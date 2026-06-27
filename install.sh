#!/usr/bin/env bash
# Madar installer — https://github.com/eslam-mahmoud/go-ai-agent
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/eslam-mahmoud/go-ai-agent/main/install.sh | bash
#   bash install.sh                 # normal install / resume
#   bash install.sh --update        # upgrade to the latest release
#   bash install.sh --update-keys   # re-prompt for credentials only
#   bash install.sh --uninstall     # remove service and binary

set -euo pipefail

# ── constants ────────────────────────────────────────────────────────────────
MADAR_HOME="${MADAR_HOME:-/opt/madar}"
BIN_PATH="$MADAR_HOME/madar"
CONFIG_PATH="$MADAR_HOME/config.yaml"
ENV_PATH="$MADAR_HOME/.env"
STATE_PATH="$MADAR_HOME/.install-state"
REPO="eslam-mahmoud/go-ai-agent"
SERVICE_NAME="madar"

# ── colours ──────────────────────────────────────────────────────────────────
BOLD="\033[1m"
GREEN="\033[32m"
YELLOW="\033[33m"
RED="\033[31m"
CYAN="\033[36m"
RESET="\033[0m"

info()    { echo -e "${CYAN}▶${RESET} $*"; }
success() { echo -e "${GREEN}✔${RESET} $*"; }
warn()    { echo -e "${YELLOW}⚠${RESET} $*"; }
error()   { echo -e "${RED}✖${RESET} $*" >&2; }
die()     { error "$*"; exit 1; }
bold()    { echo -e "${BOLD}$*${RESET}"; }

# ── state helpers ─────────────────────────────────────────────────────────────
state_get() { grep "^$1=" "$STATE_PATH" 2>/dev/null | cut -d= -f2 || true; }
state_set() {
    mkdir -p "$MADAR_HOME" 2>/dev/null || true
    if grep -q "^$1=" "$STATE_PATH" 2>/dev/null; then
        sed -i.bak "s|^$1=.*|$1=$2|" "$STATE_PATH" && rm -f "$STATE_PATH.bak"
    else
        echo "$1=$2" >> "$STATE_PATH"
    fi
}
step_done()   { state_set "$1" "done"; }
step_status() { state_get "$1"; }

# ── detect platform ───────────────────────────────────────────────────────────
detect_platform() {
    OS="$(uname -s)"
    ARCH="$(uname -m)"
    case "$OS" in
        Linux)  PLATFORM="linux" ;;
        Darwin) PLATFORM="darwin" ;;
        *)      die "Unsupported OS: $OS" ;;
    esac
    case "$ARCH" in
        x86_64)  GOARCH="amd64" ;;
        aarch64|arm64) GOARCH="arm64" ;;
        *)       die "Unsupported arch: $ARCH" ;;
    esac

    # Detect Linux package manager family.
    PKG_MANAGER=""
    if [[ "$PLATFORM" == "linux" ]]; then
        if has_cmd apt-get; then
            PKG_MANAGER="apt"
        elif has_cmd dnf; then
            PKG_MANAGER="dnf"
        elif has_cmd yum; then
            PKG_MANAGER="yum"
        else
            PKG_MANAGER="unknown"
        fi
    fi
}

# ── command helpers ───────────────────────────────────────────────────────────
require_cmd() {
    command -v "$1" &>/dev/null || die "'$1' is required but not found. Please install it and retry."
}

has_cmd() { command -v "$1" &>/dev/null; }

need_sudo() {
    if [[ "$EUID" -ne 0 ]] && ! has_cmd sudo; then
        die "This step requires root or sudo. Please run as root or install sudo."
    fi
    [[ "$EUID" -eq 0 ]] && echo "" || echo "sudo"
}

SUDO="$(need_sudo)"
run_privileged() { $SUDO "$@"; }

# ── read secret (no echo) ─────────────────────────────────────────────────────
read_secret() {
    local prompt="$1" var_name="$2" val
    while true; do
        read -rsp "  $prompt: " val; echo
        [[ -n "$val" ]] && break
        warn "Value cannot be empty."
    done
    printf -v "$var_name" '%s' "$val"
}

read_value() {
    local prompt="$1" var_name="$2" default="${3:-}" val
    local display_prompt="  $prompt"
    [[ -n "$default" ]] && display_prompt+=" [$default]"
    display_prompt+=": "
    read -rp "$display_prompt" val
    val="${val:-$default}"
    printf -v "$var_name" '%s' "$val"
}

# ── validate GitHub token ─────────────────────────────────────────────────────
validate_github_token() {
    local token="$1"
    local status
    status=$(curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: token $token" \
        "https://api.github.com/user")
    [[ "$status" == "200" ]]
}

# ── package manager helpers ───────────────────────────────────────────────────
pkg_install() {
    case "$PKG_MANAGER" in
        apt) run_privileged apt-get install -y "$@" ;;
        dnf) run_privileged dnf install -y "$@" ;;
        yum) run_privileged yum install -y "$@" ;;
        *)   die "No supported package manager found (apt/dnf/yum). Install manually: $*" ;;
    esac
}

install_nodejs_linux() {
    case "$PKG_MANAGER" in
        apt)
            curl -fsSL https://deb.nodesource.com/setup_lts.x | run_privileged bash -
            run_privileged apt-get install -y nodejs
            ;;
        dnf|yum)
            curl -fsSL https://rpm.nodesource.com/setup_lts.x | run_privileged bash -
            pkg_install nodejs
            ;;
        *)
            # Fallback: install via nvm (works on any Linux without package manager)
            warn "Unknown package manager — installing Node.js via nvm"
            export NVM_DIR="$HOME/.nvm"
            curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash
            # shellcheck source=/dev/null
            source "$NVM_DIR/nvm.sh"
            nvm install --lts
            ;;
    esac
}

install_gh_linux() {
    case "$PKG_MANAGER" in
        apt)
            curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
                | run_privileged dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
            echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
                | run_privileged tee /etc/apt/sources.list.d/github-cli.list > /dev/null
            run_privileged apt-get update && run_privileged apt-get install -y gh
            ;;
        dnf|yum)
            run_privileged "$PKG_MANAGER" config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo 2>/dev/null || \
                run_privileged "$PKG_MANAGER" install -y 'dnf-command(config-manager)' 2>/dev/null && \
                run_privileged "$PKG_MANAGER" config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo
            pkg_install gh
            ;;
        *)
            # Fallback: download gh binary directly
            warn "Unknown package manager — installing gh via binary download"
            local gh_ver
            gh_ver=$(curl -s https://api.github.com/repos/cli/cli/releases/latest \
                | grep '"tag_name"' | cut -d'"' -f4 | sed 's/^v//')
            curl -fsSL "https://github.com/cli/cli/releases/download/v${gh_ver}/gh_${gh_ver}_linux_amd64.tar.gz" \
                | run_privileged tar -xz -C /usr/local --strip-components=1
            ;;
    esac
}

# ── step: deps ────────────────────────────────────────────────────────────────
install_deps() {
    if [[ "$(step_status deps)" == "done" ]]; then
        success "Dependencies already installed (skipping)"
        return
    fi
    info "Installing dependencies…"

    # Node.js (required for Claude Code CLI)
    if ! has_cmd node; then
        info "Installing Node.js…"
        if [[ "$PLATFORM" == "linux" ]]; then
            install_nodejs_linux
        elif [[ "$PLATFORM" == "darwin" ]]; then
            has_cmd brew || die "Homebrew is required on macOS. Install from https://brew.sh"
            brew install node
        fi
    else
        success "Node.js already installed ($(node --version))"
    fi

    # git
    if ! has_cmd git; then
        info "Installing git…"
        if [[ "$PLATFORM" == "linux" ]]; then
            pkg_install git
        elif [[ "$PLATFORM" == "darwin" ]]; then
            brew install git
        fi
    else
        success "git already installed"
    fi

    # gh CLI (used to clone private repos and post comments)
    if ! has_cmd gh; then
        info "Installing gh CLI…"
        if [[ "$PLATFORM" == "linux" ]]; then
            install_gh_linux
        elif [[ "$PLATFORM" == "darwin" ]]; then
            brew install gh
        fi
    else
        success "gh CLI already installed"
    fi

    step_done deps
    success "Dependencies installed"
}

# ── step: claude CLI ──────────────────────────────────────────────────────────
install_claude() {
    if [[ "$(step_status claude_install)" == "done" ]]; then
        success "Claude Code CLI already installed (skipping)"
        return
    fi
    info "Installing Claude Code CLI…"
    run_privileged npm install -g @anthropic-ai/claude-code
    step_done claude_install
    success "Claude Code CLI installed"
}

auth_claude() {
    if [[ "$(step_status claude_auth)" == "done" ]]; then
        success "Claude Code already authenticated (skipping)"
        return
    fi
    info "Authenticating Claude Code…"
    echo ""
    bold "  A browser window will open for OAuth authentication."
    bold "  Log in with your Anthropic account (subscription required)."
    echo ""
    claude login
    step_done claude_auth
    success "Claude Code authenticated"
}

# ── step: download binary ─────────────────────────────────────────────────────
install_binary() {
    if [[ "$(step_status binary)" == "done" ]] && [[ -x "$BIN_PATH" ]]; then
        success "Madar binary already installed (skipping)"
        return
    fi
    info "Installing Madar binary…"

    # Try to download pre-built release first
    local release_url
    release_url=$(curl -s "https://api.github.com/repos/$REPO/releases/latest" \
        | grep "browser_download_url" \
        | grep "${PLATFORM}-${GOARCH}" \
        | cut -d'"' -f4 || true)

    if [[ -n "$release_url" ]]; then
        info "Downloading from release: $release_url"
        curl -fsSL "$release_url" -o "$BIN_PATH"
        chmod +x "$BIN_PATH"
    else
        # Fallback: build from source
        warn "No pre-built release found for ${PLATFORM}-${GOARCH} — building from source"
        local tmpdir
        tmpdir=$(mktemp -d)
        info "Cloning source…"
        git clone --depth=1 "https://github.com/$REPO.git" "$tmpdir/src"

        # Auto-install Go if missing — read required version from go.mod
        if ! has_cmd go; then
            local go_version
            go_version=$(grep '^go ' "$tmpdir/src/go.mod" | awk '{print $2}')
            info "Installing Go ${go_version}…"
            curl -fsSL "https://go.dev/dl/go${go_version}.linux-${GOARCH}.tar.gz" \
                | run_privileged tar -xz -C /usr/local
            export PATH="/usr/local/go/bin:$PATH"
            has_cmd go || die "Go installation failed. Install Go ${go_version}+ manually and retry."
        fi

        info "Building Madar from source (this may take a minute)…"
        local built=false
        (cd "$tmpdir/src" && \
            GOOS=linux GOARCH="${GOARCH}" \
            go build -trimpath -ldflags "-s -w" -o "$BIN_PATH" ./cmd/madar/ && \
            built=true) || true

        rm -rf "$tmpdir"
        [[ "$built" == "true" ]] || die "Build failed. Check errors above."
        chmod +x "$BIN_PATH"
    fi

    step_done binary
    success "Madar binary installed at $BIN_PATH"
}

# ── step: credentials ─────────────────────────────────────────────────────────
configure_credentials() {
    echo ""
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    bold " Configure credentials"
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""

    # Read existing values for update mode
    local existing_token existing_tg_token existing_tg_ids
    existing_token=$(grep "^GITHUB_TOKEN=" "$ENV_PATH" 2>/dev/null | cut -d= -f2 || true)
    existing_tg_token=$(grep "^TELEGRAM_BOT_TOKEN=" "$ENV_PATH" 2>/dev/null | cut -d= -f2 || true)
    existing_tg_ids=$(grep "^TELEGRAM_ALLOWED_IDS=" "$ENV_PATH" 2>/dev/null | cut -d= -f2 || true)

    # ── GitHub token ──
    echo -e "  ${BOLD}GitHub Personal Access Token${RESET}"
    echo "  Needs 'repo' scope. Create at: https://github.com/settings/tokens/new"
    if [[ -n "$existing_token" ]]; then
        echo "  Current: ${existing_token:0:8}… (press Enter to keep)"
        read -rsp "  New token (hidden): " GITHUB_TOKEN; echo
        GITHUB_TOKEN="${GITHUB_TOKEN:-$existing_token}"
    else
        read_secret "GitHub token (hidden)" GITHUB_TOKEN
    fi

    # Validate
    info "Validating GitHub token…"
    if validate_github_token "$GITHUB_TOKEN"; then
        local gh_user
        gh_user=$(curl -s -H "Authorization: token $GITHUB_TOKEN" \
            "https://api.github.com/user" | python3 -c "import sys,json; print(json.load(sys.stdin)['login'])")
        success "Token valid — authenticated as @${gh_user}"
    else
        die "GitHub token validation failed. Please check the token and try again."
    fi

    echo ""

    # ── Telegram ──
    echo -e "  ${BOLD}Telegram Bot Token${RESET}"
    echo "  Create a bot at @BotFather and paste the token here."
    if [[ -n "$existing_tg_token" ]]; then
        echo "  Current: ${existing_tg_token:0:10}… (press Enter to keep)"
        read -rsp "  New token (hidden): " TELEGRAM_BOT_TOKEN; echo
        TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-$existing_tg_token}"
    else
        read_secret "Telegram bot token (hidden)" TELEGRAM_BOT_TOKEN
    fi

    echo ""
    echo -e "  ${BOLD}Telegram Allowed Chat IDs${RESET}"
    echo "  Message @userinfobot on Telegram to get your chat ID."
    echo "  Comma-separated for multiple recipients."
    if [[ -n "$existing_tg_ids" ]]; then
        read_value "Chat IDs" TELEGRAM_ALLOWED_IDS "$existing_tg_ids"
    else
        read_value "Chat IDs (e.g. 123456789)" TELEGRAM_ALLOWED_IDS ""
    fi

    echo ""

    # Write .env
    # Preserve any extra keys that were already in .env
    local extra_keys=""
    if [[ -f "$ENV_PATH" ]]; then
        extra_keys=$(grep -v "^GITHUB_TOKEN=\|^TELEGRAM_BOT_TOKEN=\|^TELEGRAM_ALLOWED_IDS=" "$ENV_PATH" || true)
    fi
    {
        echo "GITHUB_TOKEN=$GITHUB_TOKEN"
        echo "TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN"
        echo "TELEGRAM_ALLOWED_IDS=$TELEGRAM_ALLOWED_IDS"
        [[ -n "$extra_keys" ]] && echo "$extra_keys"
    } > "$ENV_PATH"
    chmod 600 "$ENV_PATH"

    step_done credentials
    success "Credentials saved to $ENV_PATH"
}

# ── step: config ──────────────────────────────────────────────────────────────
configure_repos() {
    if [[ "$(step_status repos)" == "done" ]] && [[ -f "$CONFIG_PATH" ]]; then
        success "Config already exists (skipping — use --update-keys to edit credentials)"
        return
    fi

    echo ""
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    bold " Configure repositories"
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Which GitHub repos should Madar watch for issues?"
    echo "  Format: owner/repo (one per line, empty line to finish)"
    echo ""
    local repos=()
    while true; do
        local repo_input
        read -rp "  Repo (or Enter to finish): " repo_input
        [[ -z "$repo_input" ]] && break
        # Normalise: strip https://github.com/ prefix and trailing slashes
        repo_input=$(echo "$repo_input" \
            | sed 's|https://github\.com/||; s|http://github\.com/||; s|github\.com/||; s|/$||')
        if [[ "$repo_input" != */* ]]; then
            warn "Expected format: owner/repo — got '$repo_input'. Skipping."
            continue
        fi
        repos+=("$repo_input")
    done

    # Build repos yaml block
    local repos_yaml=""
    for r in "${repos[@]}"; do
        repos_yaml+="  - $r"$'\n'
    done
    [[ -z "$repos_yaml" ]] && repos_yaml="  # - owner/project-a"$'\n'

    # Write config.yaml
    mkdir -p "$MADAR_HOME/workspaces"  # workspaces subdir is always safe (user-owned)
    cat > "$CONFIG_PATH" <<EOF
poll_interval_seconds: 45

concurrency:
  enabled: false
  max_parallel: 1

labels:
  ready: ready
  in_progress: in-progress
  awaiting_feedback: awaiting-feedback
  done: done

repos:
${repos_yaml}
context_dir: .claude-context

claude:
  bin: ""
  output_format: stream-json
  max_turns: 40
  run_timeout: 30m
  auto_compact: false
  context_reset_threshold: 0.6
  skip_permissions: true
  max_thread_chars: 8000
  max_issue_body_chars: 4000

ci:
  enabled: false
  max_retries: 3
  poll_interval: 30s
  wait_timeout: 20m

cleanup:
  interval: 24h
  audit_log_retention: 720h
  task_retention: 2160h

db_path: $MADAR_HOME/madar.db
workspace_dir: $MADAR_HOME/workspaces
EOF

    step_done repos
    success "Config saved to $CONFIG_PATH"
}

# ── step: service ─────────────────────────────────────────────────────────────
install_service() {
    if [[ "$(step_status service)" == "done" ]]; then
        success "Service already installed (skipping)"
        return
    fi

    if [[ "$PLATFORM" == "linux" ]]; then
        install_systemd_service
    elif [[ "$PLATFORM" == "darwin" ]]; then
        install_launchd_service
    fi
    step_done service
}

install_systemd_service() {
    info "Installing systemd service…"
    run_privileged tee /etc/systemd/system/${SERVICE_NAME}.service > /dev/null <<EOF
[Unit]
Description=Madar autonomous coding agent
After=network.target

[Service]
Type=simple
User=$(logname 2>/dev/null || echo root)
WorkingDirectory=$MADAR_HOME
EnvironmentFile=$ENV_PATH
ExecStart=$BIN_PATH -config $CONFIG_PATH -log-level info
Restart=on-failure
RestartSec=10s
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
EOF
    run_privileged systemctl daemon-reload
    run_privileged systemctl enable "$SERVICE_NAME"
    run_privileged systemctl start "$SERVICE_NAME"
    success "systemd service installed and started"
    echo ""
    info "Useful commands:"
    echo "   systemctl status $SERVICE_NAME"
    echo "   journalctl -fu $SERVICE_NAME"
    echo "   systemctl restart $SERVICE_NAME"
}

install_launchd_service() {
    info "Installing launchd service (macOS)…"
    local plist="$HOME/Library/LaunchAgents/com.madar.agent.plist"
    mkdir -p "$HOME/Library/LaunchAgents"
    cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.madar.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN_PATH</string>
        <string>-config</string>
        <string>$CONFIG_PATH</string>
        <string>-log-level</string>
        <string>info</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>GITHUB_TOKEN</key>
        <string>__REPLACE_GITHUB_TOKEN__</string>
        <key>TELEGRAM_BOT_TOKEN</key>
        <string>__REPLACE_TELEGRAM_TOKEN__</string>
        <key>TELEGRAM_ALLOWED_IDS</key>
        <string>__REPLACE_TELEGRAM_IDS__</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$MADAR_HOME/madar.log</string>
    <key>StandardErrorPath</key>
    <string>$MADAR_HOME/madar.log</string>
</dict>
</plist>
EOF
    # Inject actual values from .env
    sed -i '' \
        -e "s|__REPLACE_GITHUB_TOKEN__|$(grep GITHUB_TOKEN "$ENV_PATH" | cut -d= -f2)|" \
        -e "s|__REPLACE_TELEGRAM_TOKEN__|$(grep TELEGRAM_BOT_TOKEN "$ENV_PATH" | cut -d= -f2)|" \
        -e "s|__REPLACE_TELEGRAM_IDS__|$(grep TELEGRAM_ALLOWED_IDS "$ENV_PATH" | cut -d= -f2)|" \
        "$plist"
    launchctl load "$plist"
    success "launchd service installed and started"
    echo ""
    info "Useful commands:"
    echo "   launchctl list | grep madar"
    echo "   tail -f $MADAR_HOME/madar.log"
    echo "   launchctl unload $plist && launchctl load $plist  # restart"
}

# ── uninstall ─────────────────────────────────────────────────────────────────
uninstall() {
    bold "Uninstalling Madar…"
    if [[ "$PLATFORM" == "linux" ]] && has_cmd systemctl; then
        run_privileged systemctl stop "$SERVICE_NAME" 2>/dev/null || true
        run_privileged systemctl disable "$SERVICE_NAME" 2>/dev/null || true
        run_privileged rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
        run_privileged systemctl daemon-reload
    elif [[ "$PLATFORM" == "darwin" ]]; then
        local plist="$HOME/Library/LaunchAgents/com.madar.agent.plist"
        launchctl unload "$plist" 2>/dev/null || true
        rm -f "$plist"
    fi
    echo ""
    warn "Binary and config preserved at $MADAR_HOME"
    warn "To fully remove: rm -rf $MADAR_HOME"
    success "Service removed"
}

# ── print final summary ───────────────────────────────────────────────────────
print_summary() {
    echo ""
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    bold " Madar is installed and running!"
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Binary  : $BIN_PATH"
    echo "  Config  : $CONFIG_PATH"
    echo "  Secrets : $ENV_PATH"
    echo "  DB      : $MADAR_HOME/madar.db"
    echo ""
    echo "  Next steps:"
    echo "   1. Open an issue on a watched repo and label it 'ready'"
    echo "   2. Madar will pick it up on the next poll (~45s)"
    echo "   3. Check status: $BIN_PATH -config $CONFIG_PATH -status"
    echo ""
    echo "  To update credentials later:"
    echo "   curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | bash -s -- --update-keys"
    echo ""
    echo "  To edit repos:"
    echo "   \$EDITOR $CONFIG_PATH"
    echo ""
}

# ── main ──────────────────────────────────────────────────────────────────────
main() {
    local mode="install"
    for arg in "$@"; do
        case "$arg" in
            --update-keys)  mode="update-keys" ;;
            --update)       mode="update" ;;
            --uninstall)    mode="uninstall" ;;
            --help|-h)
                echo "Usage: install.sh [--update | --update-keys | --uninstall]"
                echo "  (no args)       Install or resume a partial install"
                echo "  --update        Download and install the latest Madar release"
                echo "  --update-keys   Re-prompt for credentials only"
                echo "  --uninstall     Remove the service (keeps files)"
                exit 0
                ;;
        esac
    done

    detect_platform

    echo ""
    bold "╔═══════════════════════════════════════╗"
    bold "║   Madar Installer                     ║"
    bold "║   Autonomous coding agent             ║"
    bold "╚═══════════════════════════════════════╝"
    echo ""

    # When run via curl | bash, stdin is the pipe (the script itself).
    # Re-open stdin from the controlling terminal so interactive read
    # prompts work normally throughout the rest of the installer.
    if [[ ! -t 0 ]]; then
        exec </dev/tty
    fi

    # Create the install directory owned by the current user early so all
    # subsequent writes (state file, .env, config, binary) work without sudo.
    if [[ ! -d "$MADAR_HOME" ]]; then
        run_privileged mkdir -p "$MADAR_HOME"
        run_privileged chown "$(id -un):$(id -gn)" "$MADAR_HOME"
    fi

    case "$mode" in
        uninstall)
            uninstall
            exit 0
            ;;
        update)
            info "Updating Madar binary to the latest release…"
            if [[ ! -x "$BIN_PATH" ]]; then
                die "Madar is not installed at $BIN_PATH. Run the installer first."
            fi
            local old_version
            old_version=$("$BIN_PATH" -version 2>/dev/null || echo "unknown")
            info "Current version: $old_version"

            local release_url
            release_url=$(curl -s "https://api.github.com/repos/$REPO/releases/latest" \
                | grep "browser_download_url" \
                | grep "${PLATFORM}-${GOARCH}" \
                | cut -d'"' -f4 || true)

            if [[ -z "$release_url" ]]; then
                die "No pre-built release found for ${PLATFORM}/${GOARCH}. Check https://github.com/$REPO/releases"
            fi

            # Verify checksum before replacing binary
            local checksums_url
            checksums_url=$(curl -s "https://api.github.com/repos/$REPO/releases/latest" \
                | grep "browser_download_url" \
                | grep "checksums.txt" \
                | cut -d'"' -f4 || true)

            local tmpdir
            tmpdir=$(mktemp -d)
            info "Downloading $release_url"
            curl -fsSL "$release_url" -o "$tmpdir/madar.new"

            if [[ -n "$checksums_url" ]]; then
                curl -fsSL "$checksums_url" -o "$tmpdir/checksums.txt"
                local expected_sum
                expected_sum=$(grep "madar-${PLATFORM}-${GOARCH}" "$tmpdir/checksums.txt" | awk '{print $1}')
                if [[ -n "$expected_sum" ]]; then
                    local actual_sum
                    actual_sum=$(sha256sum "$tmpdir/madar.new" | awk '{print $1}')
                    if [[ "$expected_sum" != "$actual_sum" ]]; then
                        rm -rf "$tmpdir"
                        die "Checksum mismatch — aborting update (expected $expected_sum, got $actual_sum)"
                    fi
                    info "Checksum verified"
                fi
            fi

            # Stop service, swap binary, restart
            if [[ "$PLATFORM" == "linux" ]] && has_cmd systemctl; then
                run_privileged systemctl stop "$SERVICE_NAME" 2>/dev/null || true
            elif [[ "$PLATFORM" == "darwin" ]]; then
                launchctl unload "$HOME/Library/LaunchAgents/com.madar.agent.plist" 2>/dev/null || true
            fi

            chmod +x "$tmpdir/madar.new"
            run_privileged mv "$tmpdir/madar.new" "$BIN_PATH"
            rm -rf "$tmpdir"

            if [[ "$PLATFORM" == "linux" ]] && has_cmd systemctl; then
                run_privileged systemctl start "$SERVICE_NAME" 2>/dev/null && \
                    success "Service restarted"
            elif [[ "$PLATFORM" == "darwin" ]]; then
                launchctl load "$HOME/Library/LaunchAgents/com.madar.agent.plist" 2>/dev/null || true
                success "Service restarted"
            fi

            local new_version
            new_version=$("$BIN_PATH" -version 2>/dev/null || echo "unknown")
            success "Updated: $old_version → $new_version"
            exit 0
            ;;
        update-keys)
            info "Update mode — re-configuring credentials"
            configure_credentials
            # Restart service to pick up new .env
            if [[ "$PLATFORM" == "linux" ]] && has_cmd systemctl; then
                run_privileged systemctl restart "$SERVICE_NAME" 2>/dev/null && \
                    success "Service restarted with new credentials"
            elif [[ "$PLATFORM" == "darwin" ]]; then
                local plist="$HOME/Library/LaunchAgents/com.madar.agent.plist"
                launchctl unload "$plist" 2>/dev/null || true
                install_launchd_service
            fi
            exit 0
            ;;
        install)
            info "Platform: $PLATFORM/$GOARCH"
            info "Install directory: $MADAR_HOME"
            echo ""

            install_deps
            install_claude
            auth_claude
            install_binary
            configure_credentials
            configure_repos
            install_service
            print_summary
            ;;
    esac
}

main "$@"
