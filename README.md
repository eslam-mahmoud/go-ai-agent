# Madar — Autonomous Coding Agent

> *Madar (مدار, "orbit") — an autonomous agent that owns a project goal and orbits it: plan, build, review, verify, decide what's next, repeat.*

Madar is a single Go binary that runs on a server and acts as an autonomous software engineer. You give it a repository and a goal; it maintains a backlog, delivers one task at a time through the Claude Code CLI, and reports to you on GitHub and Telegram. No custom UI, no webhooks, no extra infrastructure.

**The permanent invariant: one project goal, one active task, one branch, one pull request, and one delivery decision at a time.** Everything below follows from it. Madar is deliberately not a swarm — sequential delivery is what makes its state auditable and its restarts safe.

---

## Table of Contents

**Getting started**
- [Prerequisites](#prerequisites)
- [Install](#install)
- [First run](#first-run)
- [Checking status](#checking-status)
- [Updating](#updating)
- [Rotating credentials](#rotating-credentials)
- [Uninstalling](#uninstalling)

**Understanding it**
- [How it works](#how-it-works)
- [Owner commands](#owner-commands)
- [Safety](#safety)
- [CI/CD feedback loop](#cicd-feedback-loop)
- [Architecture](#architecture)
- [Design decisions](#design-decisions)

**Reference**
- [Environment variables](#environment-variables)
- [Configuration reference](#configuration-reference)
- [CLI reference](#cli-reference)
- [Project structure](#project-structure)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

You need these before installing. The installer will set up the ones marked *auto*.

| Requirement | Why | Notes |
|---|---|---|
| **Linux or macOS** | Service management via systemd or launchd | Windows is not supported |
| **A Claude subscription or API access** | Madar drives the Claude Code CLI; it does not call the API directly | Authenticated with `claude login` — the credential lives in `~/.claude`, never in Madar's config |
| **Claude Code CLI** | The thing that actually writes code | *auto* — installed and authenticated during setup |
| **git** | Cloning and working in the repository | *auto* |
| **Node.js 18+** | Required by the Claude Code CLI | *auto* |
| **A GitHub repository you can write to** | Madar opens issues, pushes branches, and opens pull requests | Must be able to create branches and PRs |
| **A GitHub personal access token** | API access, with `repo` scope | Create at [github.com/settings/tokens](https://github.com/settings/tokens) |
| **A Telegram bot** *(optional)* | Progress notifications and owner commands | From [@BotFather](https://t.me/botfather). Without it Madar delivers silently |
| **~2 GB disk** | Binary, database, and the cloned repository | Grows with execution history |

Madar has **no CGO dependency** — the SQLite driver is pure Go, so the binary cross-compiles and runs without a system SQLite.

---

## Install

### One command

```bash
curl -fsSL https://raw.githubusercontent.com/eslam-mahmoud/go-ai-agent/main/install.sh | bash
```

Re-running the installer is safe and is the way to upgrade: it compares the installed binary against the latest release and only downloads when they differ, verifying the checksum before replacing anything. An up-to-date install is left alone, and a locally built `dev` binary is never replaced without asking.

The installer:

1. Installs git, Node.js, and the `gh` CLI if missing
2. Installs the Claude Code CLI and walks you through `claude login`
3. Downloads the pre-built `madar` binary, or builds from source if no release matches your platform
4. Prompts for your GitHub token and Telegram credentials, **validating each before saving**
5. Asks which repository to deliver and what the goal is, then creates the project
6. Installs and starts a service — systemd on Linux, launchd on macOS

Everything lands in `/opt/madar` (override with `MADAR_HOME`). The installer is **idempotent**: re-run it after an interruption and it resumes from a checkpoint at `/opt/madar/.install-state` rather than starting over.

### From source

```bash
git clone https://github.com/eslam-mahmoud/go-ai-agent.git
cd go-ai-agent
go build -o madar ./cmd/madar/
```

Then create `config.yaml` (see [Configuration reference](#configuration-reference)) and `.env` (see [Environment variables](#environment-variables)), and authenticate Claude:

```bash
claude login
```

---

## First run

Madar delivers **one project**. Create it, give it work, and start the daemon.

**1. Create the project.** The installer does this for you; do it manually if you built from source:

```bash
madar project create --repo owner/name --name "My Project" --goal "What done looks like"
```

**2. Add work.** Either add tasks yourself:

```bash
madar project add-task --repo owner/name --title "Add rate limiting" --goal "Requests are capped per client"
```

…or set `project.auto_initialize: true` and let the Architect propose the first backlog by reading the repository.

**3. Start it.** The installer already did this. To run it yourself:

```bash
madar
```

**4. Watch it work.** Each tick advances the project exactly one step:

```bash
madar -status
```

---

## Checking status

```bash
madar -status
```

No flags needed: Madar finds its own configuration (see [configuration discovery](#configuration-discovery)).

```
madar status
  schema version : 19
  db             : /opt/madar/madar.db
  repo           : owner/name
  project        : My Project (executing, health on-track)
  backlog        : 7 task(s)
    completed        3
    queued           3
    developing       1
  current task   : #4 Add rate limiting (developing)
    issue        : #128
    branch       : madar/issue-128
    pull request : #131
    last run     : developer completed (engine claude, model default)
```

`-status` opens the database **read-only** and does not take the daemon lock, so it is safe to run while Madar is working.

For the service itself:

```bash
systemctl status madar        # Linux
journalctl -fu madar          # Linux — follow the log
launchctl list | grep madar   # macOS
```

If you set up Telegram, `/status` in the chat answers the same question, and Madar keeps one live status message updated in place rather than flooding the channel.

---

## Updating

```bash
curl -fsSL https://raw.githubusercontent.com/eslam-mahmoud/go-ai-agent/main/install.sh | bash -s -- --update
```

This replaces the binary with the latest release and restarts the service. Your config, credentials, and database are untouched. Database migrations are ordered and applied automatically on the next start.

The binary can also update itself:

```bash
madar -update
```

**Rolling back** is a matter of reinstalling an earlier release. Migrations only move forward, so a binary older than your database will refuse to start rather than corrupt it — that refusal is deliberate.

---

## Rotating credentials

```bash
curl -fsSL https://raw.githubusercontent.com/eslam-mahmoud/go-ai-agent/main/install.sh | bash -s -- --update-keys
```

This re-prompts for the GitHub token and Telegram credentials, validates each against its API before saving, and restarts the service. Nothing else changes.

To edit them by hand instead:

```bash
$EDITOR /opt/madar/.env
systemctl restart madar
```

**The Claude credential is not in `.env`.** It is managed by the Claude Code CLI and lives in `~/.claude`. To rotate it:

```bash
claude logout && claude login
```

Note that the service runs as a specific user — authenticate as that same user, or the daemon will not see the new credential.

---

## Uninstalling

```bash
curl -fsSL https://raw.githubusercontent.com/eslam-mahmoud/go-ai-agent/main/install.sh | bash -s -- --uninstall
```

This stops and removes the service. **It deliberately leaves your data in place**, because the database is the only record of what the agent did:

| Path | What it is |
|---|---|
| `/opt/madar/madar` | The binary |
| `/opt/madar/config.yaml` | Project settings |
| `/opt/madar/.env` | GitHub and Telegram credentials |
| `/opt/madar/madar.db` | Project, backlog, and execution history |
| `/opt/madar/workspaces/` | The cloned repository |

Once you are sure you want none of it:

```bash
sudo rm -rf /opt/madar
```

Madar leaves nothing outside `/opt/madar` except the service definition the uninstaller already removed. It does **not** touch `~/.claude` — remove that with `claude logout` if you want the Claude credential gone too.

---

## How it works

Madar owns a project and advances it one step per tick.

```
            ┌──────────────────────────────────────────┐
            │            Engineering Manager           │
            │  reviews the completed task, decides     │
            │  discoveries, reorders, picks what's next│
            └────────────────┬─────────────────────────┘
                             │ selects one task
                             ▼
   ┌────────┐   ┌───────────┐   ┌──────────┐   ┌───────┐   ┌──────────┐
   │Planner │──▶│ Developer │──▶│ Reviewer │──▶│ Fixer │──▶│ Verifier │
   └────────┘   └───────────┘   └──────────┘   └───────┘   └──────────┘
     plan          write code      find issues    fix them    prove it works
                                        │            ▲
                                        └────────────┘
                                       only if findings block
                             │
                             ▼
                    task completed ──▶ back to the Manager
```

Each tick does exactly one thing:

| Project state | What the tick does |
|---|---|
| paused | nothing |
| no backlog | initializes it (`auto_initialize`), otherwise idles |
| no current task | runs a Manager review to select the next one |
| task in flight | runs the next delivery mode |
| task terminal | Manager review: decides discoveries, runs the Architect if needed, publishes, selects what's next |
| budget exhausted | stops and records `budget.exhausted` |

**Startup order is deliberate: recovery, then reconciliation, then delivery.** Recovery repairs what a crash left half-written *before* reconciliation compares it against GitHub, so a restart never reconciles the wrong thing.

### Discoveries

When a mode notices something outside its task — a bug, a missing test, a security concern — it records a *discovery* rather than acting on it. The Manager decides each one: accept it into the backlog, defer it, or reject it. This is how Madar grows its own backlog without ever losing the thread of the current task.

### What you see

- **The parent issue** on GitHub is a live dashboard: goal, backlog, progress, and the latest Manager review.
- **`.madar/project.yaml` and `.madar/plan.md`** are written into the repository, so the plan is versioned alongside the code.
- **One Telegram message**, edited in place, shows what is running right now.

### Mode outputs are durable

Every mode run is recorded, and its output stored under `<workspace_dir>/.madar/executions/`. That is how the Developer reads the Planner's plan and the Fixer reads the Reviewer's findings — across restarts, not just within one run.

---

## Owner commands

With a Telegram bot configured, these are answered in chat:

| Command | What it does |
|---|---|
| `/status` | What is running right now |
| `/project` | Goal, health, and progress |
| `/plan` | The ordered backlog |
| `/next` | What the Manager selected and why |
| `/logs` | Recent workflow events |
| `/pause` | Stop after the current step |
| `/resume` | Continue |
| `/cancel` | Abandon the current task |
| `/retry` | Re-run the failed step |

Only the Telegram IDs in `TELEGRAM_ALLOWED_IDS` may issue them — **an empty list authorizes nobody**, and Madar does not build the command surface at all rather than presenting one that refuses everything. Commands also expire (`command_max_age`) and are rate limited, with a tighter limit on the ones that change delivery.

---

## Safety

**Sandboxing.** Every mode declares a workspace permission and both provider adapters enforce it: `read-only` for Planner, Reviewer, Manager, and Architect — they cannot write files, edit code, or run commands — and `workspace-write` for Developer, Fixer, and Verifier. An unrecognized value is refused rather than ignored.

**Policy.** An optional `policy:` block adds per-command and per-path rules. They are handed to the provider as tool-permission rules, so a denied command or write is **refused at the moment of the tool call** rather than noticed afterwards, when the file has already been written. Deny always wins, and a denied path is denied to every tool that can write it.

```yaml
policy:
  commands:
    default: ask
    deny: [git push --force]
  paths:
    deny: [.env]        # denied even inside a writable root
```

Policy rules are enforced on the Claude engine. The Codex CLI has no equivalent permission surface, so a Codex deployment gets the sandbox but not the fine-grained rules.

**Budgets.** `project.budgets` bounds what one task may consume — wall-clock time, review/fix cycles, CI repair attempts, mode retries. They are measured against the immutable execution history, so a restart cannot reset them.

**`skip_permissions`** is the explicit, documented way to opt out of all of it. It is on by default in the installer's config because an autonomous agent that stops to ask cannot run unattended — turn it off if you intend to supervise each run.

---

## CI/CD feedback loop

With `ci.enabled: true`, the Verifier will not accept a task until the GitHub Actions check suite for its branch is green. A red suite sends the task back to the Fixer with the failure output, bounded by `project.budgets.max_ci_fix_cycles`.

```
Developer pushes madar/issue-N ──▶ opens PR
                                     │
                        Madar reads the check suite
                                     │
              ┌──────────────────────┼──────────────────────┐
              │ pending              │ success              │ failure
              ▼                      ▼                      ▼
           wait                Verifier accepts       back to Fixer
                                     │                with the output
                                     ▼                      │
                            Manager review          budget exhausted?
                                                             │
                                                     stop and report
```

**Why this is not a shell script.** Something has to wait for an asynchronous CI event, know which task and which provider session the failed PR belongs to, and resume that work with the test output. That requires state surviving across process restarts — which is exactly what the database is for.

Enable it with:

```yaml
ci:
  enabled: true
```

---

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                       Madar (Go binary)                      │
│                                                              │
│  ┌──────────────┐   ┌──────────────┐   ┌─────────────────┐  │
│  │ Project Loop │──▶│ Mode         │──▶│ Engine Adapter  │  │
│  │ (one step    │   │ Dispatcher   │   │ (claude / codex)│  │
│  │  per tick)   │   │ + recorder   │   └─────────────────┘  │
│  └──────┬───────┘   └──────────────┘            │           │
│         │                                       ▼           │
│         │           ┌──────────────┐   ┌─────────────────┐  │
│         ├──────────▶│ Review       │   │ Policy + Budgets│  │
│         │           │ Coordinator  │   └─────────────────┘  │
│         │           └──────────────┘                        │
│         ▼                                                   │
│  ┌──────────────┐   ┌──────────────┐   ┌─────────────────┐  │
│  │ Store        │   │ GitHub       │   │ Telegram        │  │
│  │ (SQLite)     │   │ Client       │   │ Gateway         │  │
│  └──────────────┘   └──────────────┘   └─────────────────┘  │
└──────────────────────────────────────────────────────────────┘
         │                    │
         ▼                    ▼
   madar.db            workspaces/owner/repo/   ← the cloned repository
                       .madar/executions/       ← recorded mode outputs
```

### Components

| Component | Package | Responsibility |
|---|---|---|
| **Project Loop** | `internal/projectloop/` | Advances one project one step per tick; owns the daemon's wiring |
| **Review Coordinator** | `internal/project/review.go` | Runs the Manager cycle: discoveries, backlog, architecture, selection, publication |
| **Sequential Workflow** | `internal/workflow/` | Canonical task transitions and the plan → develop → review → fix → verify chain |
| **Delivery Modes** | `internal/mode/` | Planner, Developer, Reviewer, Fixer, Verifier, Manager, Architect, and their output schemas |
| **Engine Kernel** | `internal/engine/` | Provider-neutral contracts, registry, JSON Schema validation, error taxonomy |
| **Claude Adapter** | `internal/engine/claude/` | Spawn `claude`, parse `stream-json`, enforce sandbox and policy |
| **Codex Adapter** | `internal/engine/codex/` | Run `codex exec`/`resume`, parse JSONL, enforce sandbox |
| **Execution Recorder** | `internal/execution/` | Makes mode outputs durable so later modes can read earlier ones |
| **Project Domain** | `internal/domain/` | Project, task, execution, artifact, discovery, and review records |
| **Store** | `internal/store/` | Ordered SQLite migrations and transactional persistence |
| **Policy** | `internal/policy/` | Safety rules and per-task budgets |
| **Commands** | `internal/command/` | Owner command surface with authorization, expiry, and rate limiting |
| **Workspace** | `internal/workspace/` | Clones and refreshes the repository every mode runs in |
| **GitHub Client** | `internal/github/` | Issues, comments, labels, pull requests, check suites |
| **Telegram Gateway** | `internal/telegram/` | Notifications, live status, and inbound commands |

### Task state machine

```
proposed → queued → selected → planning → developing → reviewing
                                                           │
                                    ┌──────────────────────┤
                                    ▼                      ▼
                                 fixing              verifying
                                    │                      │
                                    └──────────────────────┤
                                                           ▼
                                                       completed
                                                           │
                                                    Manager review
                                                           │
                                              ┌────────────┴────────────┐
                                              ▼                         ▼
                                        accepted (closed)         back to queued
```

Public task state is mirrored onto GitHub issue labels; the database additionally holds the states used during restart recovery. Both are updated together — neither is the sole source of truth on its own.

---

## Design decisions

### Supervise, don't reimplement
Madar does not talk to the Claude API. It drives the Claude Code CLI, which already handles tool use, context, and sessions. Madar supplies what a CLI cannot: durable state, sequencing, and the decision about what to do next.

### One task at a time
Sequential delivery is the point, not a limitation. A single active task means the state is small enough to reason about, restarts are safe, and every decision has one clear cause.

### Modes over prompts
Each stage is a mode with a declared contract and a JSON output schema validated locally. A mode that returns malformed output fails at the boundary rather than corrupting the next stage.

### Provider-neutral by construction
Claude and Codex sit behind the same interface. A mode declares what it needs; the adapter translates. That is what let the sandbox bug be found — the two adapters disagreed about the same declaration.

### Polling over webhooks
No public endpoint, no inbound firewall rule, no webhook secret to rotate. Madar reaches out; nothing reaches in.

### SQLite, single writer
`SetMaxOpenConns(1)`. Every state change is a transaction against one connection. No connection pool means no lost update, and pure-Go SQLite means no CGO and a binary that cross-compiles cleanly.

### The plan lives in the repository
`.madar/project.yaml` and `.madar/plan.md` are versioned with the code, so the plan is reviewable in a pull request rather than trapped in a database.

### Safety outside the model
A model may propose anything. Sandbox and policy decide what is allowed, and they are enforced by the process that makes the tool calls — not by asking the model to behave.

---

## Environment variables

Secrets live in `.env`, never in `config.yaml`. **Never commit `.env`** — it is gitignored.

| Variable | Required | Description |
|---|---|---|
| `GITHUB_TOKEN` | **Yes** | Personal access token with `repo` scope: read and write issues, comments, labels, branches, and pull requests. Create at [github.com/settings/tokens](https://github.com/settings/tokens). |
| `TELEGRAM_BOT_TOKEN` | No | Bot token from [@BotFather](https://t.me/botfather). Without it, Madar delivers silently — no notifications, no live status, no owner commands. |
| `TELEGRAM_ALLOWED_IDS` | No | Comma-separated Telegram user IDs permitted to receive notifications and issue commands. Get yours from [@userinfobot](https://t.me/userinfobot). **An empty list authorizes nobody.** |

```
GITHUB_TOKEN=ghp_...
TELEGRAM_BOT_TOKEN=123456:ABC-...
TELEGRAM_ALLOWED_IDS=123456789
```

The Claude credential is **not** here. It is managed by `claude login` and lives in `~/.claude`.

The GitHub token is injected into git through the process environment — it is never written to `.git/config` and never appears in a command line or process listing.

---

## Configuration reference

`config.yaml` controls behaviour and contains no secrets, so it is safe to commit.

```yaml
# The repository this daemon delivers. One project per daemon.
# Required — Madar will not start without it.
project:
  repo: owner/name
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
  bin: ""                      # path to the claude binary; empty = find on PATH
  model: ""                    # empty = provider default
  output_format: stream-json
  max_turns: 40
  run_timeout: 30m
  auto_compact: false
  context_reset_threshold: 0.6
  skip_permissions: true       # explicit opt-out of the sandbox

# Require a green check suite before the Verifier accepts a task.
ci:
  enabled: false
  max_retries: 3
  poll_interval: 30s
  wait_timeout: 20m

# Periodic database housekeeping.
cleanup:
  interval: 24h
  audit_log_retention: 720h    # 30 days
  task_retention: 2160h        # 90 days

# Repair drift against GitHub once at startup, then on this interval.
# Set interval to 0 to run only the startup pass.
reconcile:
  interval: 15m
  on_startup: true

# Safety rules handed to the provider, so a denied command or write is
# refused at the tool call. Absent = unconstrained.
# Enforced on the Claude engine; Codex has no equivalent surface.
policy:
  commands:
    default: ask               # allow | ask | deny when nothing matches
    allow: []
    deny: []
  paths:
    writable: []
    deny: []                   # denied even inside a writable root
  require_approval: []

# Bounds on owner commands, on top of TELEGRAM_ALLOWED_IDS.
telegram:
  command_max_age: 10m         # refuse commands older than this
  rate_window: 1m
  max_commands_per_window: 20
  max_control_per_window: 5    # tighter: these change delivery

db_path: /opt/madar/madar.db
workspace_dir: /opt/madar/workspaces
```

### Keys removed with v1 issue mode

Madar refuses to start on a config containing these, naming the replacement rather than ignoring the key — silence would leave you believing work was queued.

| Removed | Replacement |
|---|---|
| `repos` | `project.repo` — one managed project per daemon |
| `labels` | Removed. Task state lives in the database, not in issue labels |
| `concurrency` | Removed. Delivery is sequential by design |
| `poll_interval_seconds` | `project.interval` |
| `context_dir` | Removed. Modes read the project's own `.madar/` documents |

---

## CLI reference

### Configuration discovery

You rarely need `-config`. Madar looks for `config.yaml` in this order and uses the first that exists:

1. The path given to `-config`, if any — used verbatim, so a typo is reported rather than silently replaced
2. `$MADAR_CONFIG`
3. `./config.yaml` — the working directory comes before the install location, so a checkout under development keeps using its own config
4. `$MADAR_HOME/config.yaml` (`MADAR_HOME` defaults to `/opt/madar`)
5. `~/.config/madar/config.yaml`

**`.env` sits beside the config that was found**, not beside your working directory, unless you pass `-env` explicitly. This matters: resolving it against the working directory is how a perfectly correct `-config` ends up reporting a missing `GITHUB_TOKEN` — the config loads, the credentials do not, and the error names the wrong problem.

When nothing is found, the error lists every path it tried.

The installer links `madar` into `/usr/local/bin`, so it runs by name.

### Daemon flags

| Flag | Default | Description |
|---|---|---|
| `-config` | *discovered* | Path to the YAML configuration file |
| `-env` | *beside the config* | Path to the `.env` file; skipped if it does not exist |
| `-log-level` | `info` | `debug`, `info`, `warn`, or `error` |
| `-status` | — | Print project status and exit. Opens the database read-only, takes no lock, and needs no credentials — so it is safe while Madar is running |
| `-version` | — | Print version, commit, and build date, then exit |
| `-update` | — | Download the latest release, replace the running binary, and exit |

### Project commands

These work directly against the configured SQLite database and use the same [configuration discovery](#configuration-discovery) as the daemon. Pass `-config` after the subcommand to override it.

```bash
# Register the project this daemon delivers.
madar project create \
  --repo owner/repository \
  --name "Project name" \
  --goal "Ship the next release" \
  --scope "In-scope behaviour and constraints" \
  --release-target v2.0.0 \
  --parent-issue 123

# Inspect.
madar project list
madar project show --repo owner/repository
madar project list-tasks --repo owner/repository

# Append work to the ordered backlog.
madar project add-task \
  --repo owner/repository \
  --title "Implement the workflow" \
  --goal "Run one task from planning through delivery" \
  --priority 10 \
  --type feature \
  --blocks-release

# Write .madar/project.yaml and .madar/plan.md into the workspace.
madar project sync-files --repo owner/repository

# Create or update the parent dashboard issue on GitHub.
madar project sync-issue --repo owner/repository

# Compare database state against GitHub and report drift.
madar project reconcile --repo owner/repository
```

`sync-files` is deterministic: equal database state produces byte-identical files, with no generation timestamp, so a diff means something actually changed.

`sync-issue` owns only the content between its hidden `madar:project-dashboard` markers. Anything you write outside them is preserved exactly.

### Migrating from v1

If you have a database from a v1 installation, convert one repository into a project:

```bash
madar migrate-project --repo owner/repository
```

`--name`, `--goal`, `--scope`, `--release-target`, and `--parent-issue` override the defaults. The conversion copies issue, PR, branch, provider-session, and state history in one transaction. Repeating it is safe and reports already-mapped rows. The legacy tables are left readable — they are the only record of what v1 did.

### Running it yourself

```bash
set -a && source .env && set +a
./madar -log-level debug
```

The installer sets up a service for you. To write the unit by hand:

```ini
# /etc/systemd/system/madar.service
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
# Graceful shutdown needs room for the cleanup context to finish.
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now madar
sudo journalctl -fu madar
```

---

## Project structure

```
.
├── cmd/madar/main.go            # Entrypoint: wiring, flags, startup order
├── internal/
│   ├── app/                     # Single-daemon instance lock
│   ├── config/                  # config.yaml + .env loading and validation
│   ├── domain/                  # Project, task, execution, discovery, review
│   ├── engine/                  # Provider-neutral contract, registry, errors
│   │   ├── claude/              # Claude CLI adapter, stream-json, sandbox
│   │   └── codex/               # Codex CLI adapter, JSONL, sandbox
│   ├── execution/               # Records mode runs and stores their output
│   ├── mode/                    # Planner, Developer, Reviewer, Fixer,
│   │                            #   Verifier, Manager, Architect + schemas
│   ├── workflow/                # Task state machine and delivery sequence
│   ├── project/                 # Controllers: selection, review, backlog,
│   │                            #   discoveries, architecture, reconciliation
│   ├── projectloop/             # The delivery loop and the daemon's wiring
│   ├── projectcli/              # madar project subcommands
│   ├── projectfiles/            # .madar/project.yaml and plan.md
│   ├── projectissue/            # Parent dashboard issue rendering
│   ├── policy/                  # Safety rules and per-task budgets
│   ├── command/                 # Owner commands, authorization, rate limits
│   ├── notify/                  # Notification router and live status message
│   ├── store/                   # Ordered SQLite migrations and persistence
│   ├── github/                  # Issues, PRs, labels, check suites
│   ├── githubops/               # Idempotent GitHub operations
│   ├── telegram/                # Bot API: notifications and inbound commands
│   ├── workspace/               # Clones and refreshes the repository
│   ├── reposcan/                # Read-only repository inspection
│   ├── architecturedocs/        # AGENTS.md and architecture generation
│   ├── updater/                 # Self-update from GitHub releases
│   └── e2e/                     # End-to-end fixtures with only the provider
│                                #   and GitHub faked
├── config.yaml                  # Behaviour — safe to commit
├── .env                         # Secrets — never commit
└── workspaces/                  # Cloned repository — never commit
    └── owner/repo/
        └── .madar/executions/   # Recorded mode outputs
```

### Building and testing

```bash
go build -o madar ./cmd/madar/
go test ./...
go test -race ./...
```

Tests use hand-rolled fakes rather than a mock framework, and require no network. `internal/e2e` drives the real store, controllers, and workflow with only the provider and GitHub faked — it fails when a stage is *skipped*, not only when a stage is wrong.

---

## Troubleshooting

### `project.repo is required to run the daemon`
Madar delivers one project and cannot start without knowing which. Set `project.repo` in `config.yaml`, then create the project:

```bash
madar project create --repo owner/name --name "Name" --goal "Goal"
```

### `config uses keys removed with v1 issue mode`
Your config predates the removal of issue mode. The error names each stale key and its replacement — see [keys removed with v1 issue mode](#keys-removed-with-v1-issue-mode).

### `no v2 project exists for "owner/name"`
The config names a repository that has no project row. Create it with `madar project create`, or check for a typo against `madar project list`.

### `no config.yaml found`
Madar lists every path it looked in. Put a config at one of them, pass `-config <path>`, or set `MADAR_HOME` to your install directory (`/opt/madar` by default).

### `claude binary not found`
Install it with `npm install -g @anthropic-ai/claude-code`, then `claude login`. If it lives somewhere unusual, set `claude.bin` in `config.yaml`.

### `GITHUB_TOKEN is required`
Add `GITHUB_TOKEN=ghp_...` to `.env`. It needs `repo` scope. The error names the `.env` path Madar actually used — if that is not the file you edited, that is the real problem.

### Nothing is happening
Run `madar -status`. Common causes, in the order worth checking:

- **The backlog is empty.** Add a task, or set `project.auto_initialize: true`.
- **The project is paused.** `/resume` in Telegram, or check `madar project show`.
- **A budget is exhausted.** The log records `budget.exhausted` with the reason.
- **An architecture risk is open.** Task selection is deliberately blocked until the Manager resolves it.

### Workspace clone failing on a private repository
`GITHUB_TOKEN` needs `repo` scope. Madar authenticates over HTTPS with the token supplied through git's environment configuration — it is never written to `.git/config`.

### `database locked`
The daemon takes `<db_path>.lock` before opening SQLite. A second daemon fails immediately and reports the PID of the lock holder. Check it with `ps -p <pid>`; do not delete the lock file while that process is alive. The file remaining after shutdown is normal — an OS advisory lock, not the file's existence, determines ownership. `madar -status` opens the database read-only and works while the daemon runs.

### A task is stuck mid-delivery
On startup Madar marks unfinished executions interrupted and re-derives what to do. `madar -status` shows the current task and its last run. If a provider session ID was lost, Madar asks rather than guessing.
