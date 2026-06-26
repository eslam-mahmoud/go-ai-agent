# Madar — Autonomous Coding Agent

> *Madar (مدار, "orbit") — a single autonomous agent that loops continuously, pulling work from a GitHub Issues board and running it through Claude Code.*

Madar is a Go binary that runs on a server (e.g. an EC2 instance) and acts as an autonomous software engineer. It polls GitHub Issues for tasks, executes them using the Claude Code CLI, posts results back as comments, and notifies you over Telegram when it needs your input. You manage work by creating and labelling GitHub Issues — no custom UI, no webhooks, no extra infrastructure.

---

## Table of Contents

- [How It Works](#how-it-works)
- [CI/CD Feedback Loop](#cicd-feedback-loop)
- [Architecture](#architecture)
- [Design Decisions](#design-decisions)
- [Installation](#installation)
- [Environment Variables](#environment-variables)
- [Configuration Reference](#configuration-reference)
- [GitHub Issue Workflow](#github-issue-workflow)
- [Running Madar](#running-madar)
- [Project Structure](#project-structure)

---

## How It Works

1. You open a GitHub Issue describing a task and label it `ready`.
2. Madar picks it up on the next poll (every 15–60 seconds), transitions it to `in-progress`, and runs Claude Code against a local clone of the repo.
3. If Claude can complete the task, it does so — commits the changes, posts a summary comment, and moves the issue to `done`.
4. If Claude needs clarification, it posts a question on the issue, moves it to `awaiting-feedback`, and sends you a Telegram message with a direct link. You reply on GitHub. Madar picks up the reply and resumes the exact same Claude session.

```
GitHub Issue (ready)
       │
       ▼
  Madar polls
       │
       ▼
  Claude Code runs in workspace
       │
   ┌───┴───┐
   │       │
 done   needs input
   │       │
   ▼       ▼
 comment  comment + Telegram
 "done"   "awaiting-feedback"
           │
      you reply on GitHub
           │
           ▼
      Madar resumes session
           │
           ▼
         done
```

---

## CI/CD Feedback Loop

When `ci.enabled: true`, Madar goes beyond just running Claude — it watches the GitHub Actions check suite after Claude opens a PR and automatically re-invokes Claude with the failure output if CI is red. This loops until green or a retry cap is hit, then escalates to you.

```
Claude commits → pushes branch madar/issue-N → opens PR
                                                    │
                                     Madar polls check suite (every 30s)
                                                    │
                          ┌─────────────────────────┼──────────────────────┐
                          │ pending                  │ success              │ failure
                          │                          │                      │
                        skip                   finalize                increment retries
                                              label: done            resume Claude session
                                              close issue            with failure output
                                              Telegram ✅                    │
                                                               ci_state = waiting
                                                               (loop repeats)
                                                                      │
                                                           retries > max_retries
                                                                      │
                                                        escalate → awaiting-feedback
                                                          post question + Telegram 🤔
```

**What Madar stores per task:** `pr_number`, `ci_state` (`waiting` / `passed` / `failed` / `gave_up`), `ci_retries` — all in SQLite so the state survives restarts.

**Why this is irreplaceable:** The CLI is a one-shot tool. Something has to wait for an async CI event, look up which Claude session corresponds to the failed PR, and resume it with the test output. That requires persistent state no bash script can provide across invocations.

### Enabling CI

```yaml
ci:
  enabled: true
  max_retries: 3       # re-invoke Claude up to N times on failure
  poll_interval: 30s   # how often to check check suite status
  wait_timeout: 20m    # give up waiting for CI after this long
```

Claude is instructed to create a branch named exactly `madar/issue-<N>` and include `PR: #<number>` in its response — Madar parses this to start watching the right branch.

---

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                    Madar (Go binary)                    │
│                                                         │
│  ┌─────────────┐   ┌──────────────┐   ┌─────────────┐  │
│  │ Orchestrator│   │ Claude Runner│   │ GitHub      │  │
│  │ (poll loop) │──▶│ (CLI wrapper)│   │ Client      │  │
│  └─────────────┘   └──────────────┘   └─────────────┘  │
│         │                                    │          │
│         ▼                                    ▼          │
│  ┌─────────────┐                    ┌─────────────────┐ │
│  │   Store     │                    │ Telegram        │ │
│  │  (SQLite)   │                    │ Gateway         │ │
│  └─────────────┘                    └─────────────────┘ │
└─────────────────────────────────────────────────────────┘
         │                    │
         ▼                    ▼
   ./madar.db         workspaces/
                      owner/repo/   ← cloned git repos
```

### Components

| Component | File | Responsibility |
|---|---|---|
| **Orchestrator** | `internal/orchestrator/loop.go` | Poll loop, task state machine, concurrency guard |
| **Claude Runner** | `internal/claude/runner.go` | Spawn `claude` CLI, parse `stream-json` output, detect clarifications |
| **GitHub Client** | `internal/github/client.go` | List issues, post comments, transition labels |
| **Store** | `internal/store/store.go` | SQLite: task ↔ session mapping, audit log |
| **Telegram Gateway** | `internal/telegram/gateway.go` | Send notifications to allowed chat IDs |
| **Config** | `internal/config/config.go` | Load `config.yaml` + `.env`, apply defaults |

### Task State Machine

```
ready ──▶ in-progress ──▶ done
                │
                ▼
         awaiting-feedback
                │
           (you reply)
                │
                ▼
          in-progress ──▶ done
```

State is expressed entirely through GitHub Issue labels. The SQLite store holds the session ID and clarification timestamp so Madar knows which Claude session to resume and which comments are human replies.

### Claude Code Integration

Madar drives the `claude` CLI in headless print mode:

```bash
# First run — establish session
claude -p "<rendered issue>" \
  --session-id <uuid> \
  --output-format stream-json --verbose \
  --max-turns 40 \
  --dangerously-skip-permissions

# Resume after human reply
claude -p "Maintainer answered: <reply>. Continue." \
  --resume <uuid> \
  --output-format stream-json --verbose
```

Each task maps to exactly one Claude Code session (a UUID stored in SQLite). The session accumulates the full conversation history; on resume Madar appends only the new human reply as the next turn.

**Clarification detection:** The first-run prompt instructs Claude to respond with `NEEDS_CLARIFICATION: <question>` if it cannot proceed. Madar checks the `result` event's text for this prefix.

---

## Design Decisions

### Supervise, don't reimplement
Madar wraps the `claude` CLI rather than reimplementing agentic behavior. Claude Code already handles file edits, shell execution, tool permissions, and session memory. Madar provides the loop, board integration, human-in-the-loop channel, and lifecycle management.

### GitHub Issues as the board
No custom Kanban UI — GitHub Issues already has search, filtering, labels, comments, and notifications. State transitions are labels; priority is issue ordering. This keeps the system simple and co-locates task tracking with code review (PRs).

### Polling over webhooks
Polling requires no inbound networking — the EC2 instance needs no open ports. For a single-operator setup the latency (15–60 seconds) is acceptable. Webhooks are a natural v2 upgrade.

### Single-threaded by default
One active task at a time. The concurrency guard uses the board itself as the lock — if any issue is `in-progress` or `awaiting-feedback`, Madar waits. Parallel execution requires workspace isolation (git worktrees or separate clones) and is a config switch away.

### Session IDs pre-assigned
Madar mints a UUIDv4 at claim-time and stores it before launching Claude. This means the task ↔ session link exists in SQLite even if the process crashes mid-run — the session can be inspected or resumed without re-running from scratch.

### `--dangerously-skip-permissions` as explicit opt-in
By default Claude Code gates every tool call interactively. A headless agent cannot respond to permission prompts, so this flag is required for autonomous operation. It is surfaced as an explicit config key (`skip_permissions: true`) so the trade-off is visible. For untrusted repos or shared machines, leave it `false` and configure an allowlist instead.

### SQLite over a network database
One instance, one file, no operational overhead. SQLite handles the write load of a single-threaded agent trivially. `modernc.org/sqlite` is used (pure Go, no CGO) so the binary cross-compiles cleanly for Linux without extra toolchain dependencies.

### Context in repo, not just in session
Durable context lives in `CLAUDE.md` (committed, always loaded) and `.claude-context/` (per-task notes written by Claude). Claude Code session transcripts are treated as disposable working memory. This means any session can be reconstructed from disk + GitHub, and a crashed session doesn't lose task knowledge.

---

## Installation

### Prerequisites

| Dependency | Notes |
|---|---|
| **Go 1.25+** | To build from source |
| **Claude Code CLI** | `npm install -g @anthropic-ai/claude-code` — requires a Claude subscription |
| **git** | For cloning project workspaces |
| **gh** (optional) | GitHub CLI, useful for PR creation in later versions |

### Build

```bash
git clone git@github.com:eslam-mahmoud/go-ai-agent.git
cd go-ai-agent
go build -o madar ./cmd/madar/
```

The binary is self-contained. Copy it anywhere.

### Set up workspaces

Madar runs Claude Code inside a local clone of each managed repository. Create the workspace directory and clone your repos:

```bash
mkdir -p workspaces/owner
git clone git@github.com:owner/your-repo.git workspaces/owner/your-repo
```

The path convention is `workspaces/<owner>/<repo>` — this is derived automatically from `config.yaml`'s `repos` list and the `workspace_dir` setting.

### Authenticate Claude Code

Claude Code uses your subscription credentials. On the machine that will run Madar:

```bash
claude login
```

Follow the OAuth device flow (authorize in a browser). The credential is stored in `~/.claude` and persists across restarts.

### Create GitHub labels

Madar needs four labels on each managed repo. Create them once:

```bash
for label in "ready:0075ca" "in-progress:e4e669" "awaiting-feedback:d93f0b" "done:0e8a16"; do
  name="${label%%:*}"; color="${label##*:}"
  gh label create "$name" --color "$color" --repo owner/your-repo
done
```

---

## Environment Variables

Store these in a `.env` file (never commit it) or export them in your environment. The `.env` file is loaded automatically when you pass `-env .env` to the binary.

| Variable | Required | Description |
|---|---|---|
| `GITHUB_TOKEN` | **Yes** | Personal access token with `repo` scope (read issues, write comments, manage labels). Create at [github.com/settings/tokens](https://github.com/settings/tokens). |
| `TELEGRAM_BOT_TOKEN` | **Yes** | Bot token from [@BotFather](https://t.me/botfather). Used to send clarification and completion notifications. |
| `TELEGRAM_ALLOWED_IDS` | **Yes** | Comma-separated list of Telegram chat/user IDs that will receive notifications. Get yours by messaging [@userinfobot](https://t.me/userinfobot). |

Example `.env`:

```
GITHUB_TOKEN=ghp_...
TELEGRAM_BOT_TOKEN=123456:ABC-...
TELEGRAM_ALLOWED_IDS=123456789,987654321
```

The Claude subscription credential is **not** stored here — it is managed by `claude login` and lives in `~/.claude`.

---

## Configuration Reference

`config.yaml` controls agent behaviour. Safe to commit (contains no secrets).

```yaml
# How often to poll GitHub for new work (seconds)
poll_interval_seconds: 45

# Concurrency — v1 is single-threaded
concurrency:
  enabled: false       # set true to allow parallel tasks
  max_parallel: 1      # raise when workspace isolation is in place

# GitHub Issue label names (must match what's on the repo)
labels:
  ready: ready
  in_progress: in-progress
  awaiting_feedback: awaiting-feedback
  done: done

# Repositories to watch (owner/repo format)
repos:
  - owner/project-a
  - owner/project-b

# Directory name inside each repo where Claude writes context files
context_dir: .claude-context

claude:
  output_format: stream-json   # always stream-json (required for session IDs)
  max_turns: 40                # cap agentic turns per invocation; overflow = error
  run_timeout: 30m             # kill the claude process after this wall time
  auto_compact: false          # Madar manages context via handoff files + session rotation
  context_reset_threshold: 0.6 # fraction of model context window at which to rotate session
  skip_permissions: true       # --dangerously-skip-permissions (required for headless operation)

# CI/CD feedback loop — watch check suites and auto-retry on failure
ci:
  enabled: false       # set true to enable
  max_retries: 3       # re-invoke Claude up to N times on CI failure
  poll_interval: 30s   # how often to poll check suite status
  wait_timeout: 20m    # give up waiting for CI after this wall time

# Local paths
db_path: /opt/madar/madar.db
workspace_dir: /opt/madar/workspaces
```

---

## GitHub Issue Workflow

### Creating a task

1. Open an issue in any repo listed under `repos:`.
2. Write a clear title and description — this is the prompt Claude receives.
3. Apply the `ready` label.

Madar picks it up on the next poll.

### Label lifecycle

| Label | Meaning |
|---|---|
| `ready` | Waiting to be picked up |
| `in-progress` | Claude is actively working |
| `awaiting-feedback` | Claude posted a question; waiting for your reply |
| `done` | Task completed |

Transitions happen automatically. You only ever set `ready` manually.

### Replying to a clarification

When Madar needs input, it posts a comment like:

> 🤔 **Madar needs your input before continuing:**
> Should I use per-IP or per-user rate limiting?

And you receive a Telegram message with a direct link to that comment.

Reply in a **new comment** on the same issue. Madar detects comments posted after its question, resumes the Claude session with your reply, and continues.

---

## Running Madar

### Locally (development / testing)

```bash
set -a && source .env && set +a
./madar -config config.yaml -env .env -log-level debug
```

### As a systemd service (EC2 / production)

Create `/etc/systemd/system/madar.service`:

```ini
[Unit]
Description=Madar autonomous coding agent
After=network.target

[Service]
Type=simple
User=madar
WorkingDirectory=/opt/madar
EnvironmentFile=/opt/madar/.env
ExecStart=/opt/madar/madar -config /opt/madar/config.yaml -log-level info
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now madar
sudo journalctl -fu madar   # follow logs
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-config` | `config.yaml` | Path to the YAML configuration file |
| `-env` | `.env` | Path to the .env file (skipped if the file doesn't exist) |
| `-log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

---

## Project Structure

```
.
├── cmd/
│   └── madar/
│       └── main.go              # Binary entrypoint
├── internal/
│   ├── config/
│   │   ├── config.go            # Config loading (YAML + .env)
│   │   └── config_test.go
│   ├── store/
│   │   ├── store.go             # SQLite task/session store
│   │   └── store_test.go
│   ├── github/
│   │   ├── client.go            # GitHub Issues API client
│   │   ├── client_test.go
│   │   └── checks.go            # GitHub Actions check suite polling
│   ├── claude/
│   │   ├── runner.go            # claude CLI wrapper + stream-json parser
│   │   └── runner_test.go
│   ├── telegram/
│   │   ├── gateway.go           # Telegram Bot API notifications
│   │   └── gateway_test.go
│   └── orchestrator/
│       ├── loop.go              # Main poll loop + task state machine
│       ├── loop_test.go
│       ├── ci.go                # CI/CD feedback loop state machine
│       ├── ci_test.go
│       ├── workspace.go         # Auto-clone workspace repos on startup
│       └── workspace_test.go
├── config.yaml                  # Agent behaviour (safe to commit)
├── .env                         # Secrets — never commit
└── workspaces/                  # Cloned repos — never commit
    └── owner/
        └── repo/
```
