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

# confirm_default_no asks a yes/no question that defaults to no. It is used
# only where the safe answer is to change nothing — a non-interactive run
# (curl | bash with no terminal) therefore keeps what is already there.
confirm_default_no() {
    local answer=""
    if [[ -t 0 ]]; then
        read -rp "  $1 [y/N]: " answer
    fi
    [[ "$answer" =~ ^[Yy]$ ]]
}

# ── version helpers ───────────────────────────────────────────────────────────

# installed_version prints the version of the binary already on disk, or an
# empty string when there is none. "unknown" is deliberately distinct from
# empty: a binary that will not report its version is still a binary, and the
# two cases are handled differently.
installed_version() {
    [[ -x "$BIN_PATH" ]] || return 0
    local raw
    raw=$("$BIN_PATH" -version 2>/dev/null) || { echo "unknown"; return 0; }
    # `madar v0.2.0 (commit abc1234, built …)` → `v0.2.0`
    local parsed
    parsed=$(echo "$raw" | awk '{print $2}')
    echo "${parsed:-unknown}"
}

# RELEASE_TAG and RELEASE_URL are filled by fetch_release_info.
RELEASE_TAG=""
RELEASE_URL=""
RELEASE_CHECKSUMS_URL=""

# fetch_release_info resolves the latest release once and caches it, so the
# rest of the installer does not make the same API call three times.
#
# It distinguishes three outcomes, because collapsing them is how a rate-limited
# API turns into a surprise 3-minute source build:
#   0  a release exists and has an asset for this platform
#   1  the API call itself failed (network, rate limit)
#   2  the API answered, but has no asset for this platform
fetch_release_info() {
    [[ -n "$RELEASE_TAG" ]] && return 0
    local body
    body=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null) || return 1
    [[ -n "$body" ]] || return 1

    RELEASE_TAG=$(echo "$body" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
    RELEASE_URL=$(echo "$body" \
        | grep "browser_download_url" \
        | grep "madar-${PLATFORM}-${GOARCH}" \
        | head -1 | cut -d'"' -f4)
    RELEASE_CHECKSUMS_URL=$(echo "$body" \
        | grep "browser_download_url" \
        | grep "checksums.txt" \
        | head -1 | cut -d'"' -f4)

    [[ -n "$RELEASE_URL" ]] || return 2
    return 0
}

# download_verified_binary fetches a release asset and checks it against the
# published checksum before it is allowed anywhere near $BIN_PATH. Both the
# install and update paths go through here; only update used to verify.
download_verified_binary() {
    local dest="$1"
    local tmpdir
    tmpdir=$(mktemp -d)
    curl -fsSL "$RELEASE_URL" -o "$tmpdir/madar.new" || {
        rm -rf "$tmpdir"; return 1
    }

    if [[ -n "$RELEASE_CHECKSUMS_URL" ]]; then
        if curl -fsSL "$RELEASE_CHECKSUMS_URL" -o "$tmpdir/checksums.txt"; then
            local expected actual
            expected=$(grep "madar-${PLATFORM}-${GOARCH}" "$tmpdir/checksums.txt" | awk '{print $1}')
            if [[ -n "$expected" ]]; then
                actual=$(checksum_of "$tmpdir/madar.new")
                if [[ "$expected" != "$actual" ]]; then
                    rm -rf "$tmpdir"
                    die "Checksum mismatch for madar-${PLATFORM}-${GOARCH}. Expected $expected, got $actual. Refusing to install."
                fi
                success "Checksum verified"
            fi
        else
            warn "Could not fetch checksums.txt — installing without verification"
        fi
    fi

    run_privileged mv "$tmpdir/madar.new" "$dest" 2>/dev/null || mv "$tmpdir/madar.new" "$dest"
    chmod +x "$dest"
    rm -rf "$tmpdir"
    return 0
}

# checksum_of works on both Linux (sha256sum) and macOS (shasum).
checksum_of() {
    if has_cmd sha256sum; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# ── step: download binary ─────────────────────────────────────────────────────
install_binary() {
    local current
    current=$(installed_version)

    local fetch_status=0
    fetch_release_info || fetch_status=$?

    if [[ -n "$current" ]]; then
        info "Installed version: $current"
    fi
    case "$fetch_status" in
        0) info "Latest release   : $RELEASE_TAG" ;;
        1) warn "Could not reach the GitHub release API (network or rate limit)" ;;
        2) warn "No release asset for ${PLATFORM}-${GOARCH} (latest is ${RELEASE_TAG:-unknown})" ;;
    esac

    # Decide against the version actually on disk, not against an install-state
    # marker. Keying off the marker is how an install that predates a config
    # change gets "already installed (skipping)" forever, leaving a daemon that
    # cannot start and no obvious way out.
    if [[ -n "$current" ]]; then
        if [[ "$current" == "dev" ]]; then
            # A locally built binary is somebody's work in progress. Replacing
            # it silently would discard it without a word.
            warn "A locally built (dev) binary is installed at $BIN_PATH"
            if [[ $fetch_status -eq 0 ]] && confirm_default_no "Replace it with release $RELEASE_TAG?"; then
                info "Replacing the dev build with $RELEASE_TAG…"
            else
                success "Keeping the existing dev build"
                step_done binary
                link_binary_onto_path
                return
            fi
        elif [[ $fetch_status -eq 1 ]]; then
            # Cannot tell what is current, but something runnable is installed.
            warn "Keeping the installed binary ($current) — could not check for updates"
            step_done binary
            link_binary_onto_path
            return
        elif [[ $fetch_status -eq 2 ]]; then
            # The check succeeded; there is simply nothing prebuilt to install.
            # Saying "could not check" here would be a lie, and would hide that
            # an upgrade exists and needs a source build to get it.
            warn "Keeping the installed binary ($current) — no prebuilt ${PLATFORM}-${GOARCH} asset in ${RELEASE_TAG:-the latest release}"
            warn "To build ${RELEASE_TAG:-it} from source, remove $BIN_PATH and re-run"
            step_done binary
            link_binary_onto_path
            return
        elif [[ "$current" == "$RELEASE_TAG" ]]; then
            success "Madar $current is already the latest release (skipping download)"
            step_done binary
            link_binary_onto_path
            return
        elif [[ "$current" == "unknown" ]]; then
            # A binary that will not report its version is treated as outdated:
            # upgrading unnecessarily is cheap, failing to upgrade is not.
            warn "Installed binary does not report a version — upgrading to $RELEASE_TAG"
        else
            info "Upgrading $current → $RELEASE_TAG"
        fi
    fi

    info "Installing Madar binary…"

    if [[ $fetch_status -eq 0 ]]; then
        info "Downloading $RELEASE_URL"
        download_verified_binary "$BIN_PATH" || die "Download failed. Check your connection and retry."
    else
        # Fallback: build from source. Reached when there is no asset for
        # this platform, or the release API could not be consulted at all.
        warn "Building from source (no usable release for ${PLATFORM}-${GOARCH})"
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
    link_binary_onto_path
}

# link_binary_onto_path makes `madar` runnable by name. Without it every
# command is a full path, and since the binary also discovers its own config,
# this is what turns `/opt/madar/madar -config /opt/madar/config.yaml -status`
# into `madar -status`.
link_binary_onto_path() {
    local target=""
    for candidate in /usr/local/bin /usr/bin; do
        if [[ -d "$candidate" ]]; then
            target="$candidate/madar"
            break
        fi
    done
    if [[ -z "$target" ]]; then
        warn "No directory on PATH to link into; run Madar as $BIN_PATH"
        return
    fi
    if [[ -e "$target" && ! -L "$target" ]]; then
        # Something that is not our symlink is already there. Replacing it
        # would be destroying a file we did not create.
        warn "$target exists and is not a symlink — leaving it alone"
        warn "Run Madar as $BIN_PATH, or link it yourself"
        return
    fi
    if run_privileged ln -sf "$BIN_PATH" "$target"; then
        success "Linked $target → $BIN_PATH — run it as: madar -status"
    else
        warn "Could not link into $target; run Madar as $BIN_PATH"
    fi
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
configure_project() {
    if [[ "$(step_status project)" == "done" ]] && [[ -f "$CONFIG_PATH" ]]; then
        success "Config already exists (skipping — use --update-keys to edit credentials)"
        return
    fi

    echo ""
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    bold " Configure the project"
    bold "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Madar delivers one project at a time — one goal, one task,"
    echo "  one branch, one pull request. Which repository is it?"
    echo ""

    local repo=""
    while [[ -z "$repo" ]]; do
        read -rp "  Repository (owner/name): " repo
        # Accept a pasted URL as readily as owner/name.
        repo=$(echo "$repo" \
            | sed 's|https://github\.com/||; s|http://github\.com/||; s|github\.com/||; s|/$||; s|\.git$||')
        if [[ "$repo" != */* ]]; then
            warn "Expected owner/name — got '$repo'."
            repo=""
        fi
    done

    local project_name
    read -rp "  Project name [${repo##*/}]: " project_name
    project_name="${project_name:-${repo##*/}}"

    echo ""
    echo "  What does 'done' look like? Madar's manager reviews progress"
    echo "  against this goal after every task."
    local project_goal=""
    while [[ -z "$project_goal" ]]; do
        read -rp "  Goal: " project_goal
    done

    mkdir -p "$MADAR_HOME/workspaces"
    cat > "$CONFIG_PATH" <<EOF
# The repository this daemon delivers. One project per daemon.
project:
  repo: $repo
  # Let the Architect propose the first backlog when none exists.
  auto_initialize: false
  # How often to advance the project by one step.
  interval: 30s
  # Bounds on one task. Every zero means unlimited.
  budgets:
    max_task_duration: 0s
    max_review_fix_cycles: 0
    max_ci_fix_cycles: 0
    max_mode_retries: 0

claude:
  bin: ""
  output_format: stream-json
  max_turns: 40
  run_timeout: 30m
  auto_compact: false
  context_reset_threshold: 0.6
  skip_permissions: true

# Watch the GitHub Actions check suite before accepting a task.
ci:
  enabled: false
  max_retries: 3
  poll_interval: 30s
  wait_timeout: 20m

cleanup:
  interval: 24h
  audit_log_retention: 720h
  task_retention: 2160h

# Repair drift against GitHub once at startup, then on this interval.
# Set interval to 0 to run only the startup pass.
reconcile:
  interval: 15m
  on_startup: true

# Safety rules handed to the provider, so a denied command or write is
# refused at the tool call. Empty here means unconstrained.
policy:
  commands:
    default: ask
    allow: []
    deny: []
  paths:
    writable: []
    deny: []
  require_approval: []

# Bounds on owner commands. Only the Telegram IDs in TELEGRAM_ALLOWED_IDS
# may issue them; these limits apply on top of that.
telegram:
  command_max_age: 10m
  rate_window: 1m
  max_commands_per_window: 20
  max_control_per_window: 5

db_path: $MADAR_HOME/madar.db
workspace_dir: $MADAR_HOME/workspaces
EOF

    success "Config saved to $CONFIG_PATH"

    # Create the project row. Without it the daemon has nothing to deliver and
    # refuses to start, so this is part of installing rather than a later step
    # the user has to discover.
    info "Creating the project…"
    if MADAR_CONFIG="$CONFIG_PATH" "$BIN_PATH" project create \
        --repo "$repo" --name "$project_name" --goal "$project_goal"; then
        success "Project created"
    else
        warn "Could not create the project automatically."
        warn "Run this once the credentials are right:"
        echo "   madar project create --repo $repo \\"
        echo "     --name '$project_name' --goal '$project_goal'"
    fi

    state_set project_repo "$repo"
    step_done project
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
    for candidate in /usr/local/bin/madar /usr/bin/madar; do
        if [[ -L "$candidate" ]]; then
            run_privileged rm -f "$candidate"
            success "Removed $candidate"
        fi
    done
    echo ""
    success "Service removed and stopped"
    echo ""
    warn "Your data is still on disk at $MADAR_HOME:"
    echo "    madar          the binary"
    echo "    config.yaml    project settings"
    echo "    .env           GitHub and Telegram credentials"
    echo "    madar.db       project, backlog, and execution history"
    echo "    workspaces/    the cloned repository"
    echo ""
    echo "  Nothing above is deleted, because the database is the only record"
    echo "  of what the agent did. To remove it all once you are sure:"
    echo "    rm -rf $MADAR_HOME"
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
    echo "   1. Add work to the backlog:"
    echo "      $BIN_PATH project add-task -config $CONFIG_PATH \\"
    echo "        --repo $(state_get project_repo) --title 'First task' --goal 'What done looks like'"
    echo "   2. Madar picks it up on the next tick (~30s) and advances it one step at a time"
    echo "   3. Watch it work:"
    echo "      $BIN_PATH -config $CONFIG_PATH -status"
    echo ""
    echo "  Useful commands:"
    echo "   Backlog        : $BIN_PATH project list-tasks -config $CONFIG_PATH --repo $(state_get project_repo)"
    echo "   Update Madar   : curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | bash -s -- --update"
    echo "   Rotate keys    : curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | bash -s -- --update-keys"
    echo "   Uninstall      : curl -fsSL https://raw.githubusercontent.com/$REPO/main/install.sh | bash -s -- --uninstall"
    echo "   Edit config    : \$EDITOR $CONFIG_PATH"
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
                echo "  (no args)       Install, resume a partial install, or"
                echo "                  upgrade an outdated binary to the latest release"
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
            configure_project
            install_service
            print_summary
            ;;
    esac
}

main "$@"
