# Madar — Project Memory for Claude Code

This file loads at the start of every Claude Code session on this repo.
It captures the conventions, invariants, and guardrails Claude must follow
when working on Madar itself.

## What Madar Is

A Go binary that acts as an autonomous coding agent. It polls GitHub Issues
for tasks, runs them through the `claude` CLI in headless mode, and posts
results back. The spec lives in `project.md`.

## Build & Test

```bash
go build -o madar ./cmd/madar/        # build
go test ./...                          # all tests (no network required)
go test ./internal/orchestrator/... -v # specific package
```

All tests must pass before pushing. Tests use fakes (not mocks) for external
interfaces — see `loop_test.go` for the pattern.

## Package Layout

```
cmd/madar/main.go           — entrypoint, wires all packages together
internal/config/            — YAML + .env loading
internal/store/             — SQLite (modernc.org/sqlite, pure Go, no CGO)
internal/github/            — GitHub API client + check suite polling
internal/claude/            — claude CLI wrapper, stream-json parser, prompts
internal/telegram/          — Telegram Bot API notifications
internal/orchestrator/      — poll loop, task state machine, CI feedback loop
```

## Branch & PR Convention

When working on this repo via GitHub Issues, **always**:

1. Create a branch named exactly `madar/issue-<N>` (e.g. `madar/issue-42`).
2. Commit your changes to that branch and push it.
3. Open a pull request and include `PR: #<number>` on its own line in your
   final response so the CI watcher can detect it.

## Key Invariants

- **Single-writer SQLite.** `db.SetMaxOpenConns(1)` — never open a second
  connection or run concurrent writes.
- **Never commit `.env`.** Secrets live in `.env` (gitignored). The config
  split is `.env` for secrets, `config.yaml` for behaviour.
- **Never commit `workspaces/`.** Cloned repos live under `workspaces/`
  (gitignored). Do not add workspace content to git.
- **Fakes over mocks.** Tests implement the full interface with a hand-rolled
  struct — no mock frameworks. See `fakeGitHub` in `loop_test.go`.
- **Table-driven tests** for pure functions; behaviour tests for state machines.
- **No CGO.** `modernc.org/sqlite` is pure Go. Keep it that way so the binary
  cross-compiles cleanly for Linux AMD64 (EC2 target).

## Task State Machine

```
ready → in-progress → done (closed)
              ↓
       awaiting-feedback  ←→  in-progress
```

State is expressed as GitHub Issue labels. SQLite holds session IDs and
timestamps. Never transition state without updating both.

## Adding a New Enhancement

1. Add config fields to `internal/config/config.go` with defaults in
   `applyDefaults`.
2. Add store columns in `internal/store/store.go` — use `CREATE TABLE IF NOT
   EXISTS` for new tables; add accessors alongside.
3. Add the GitHub/Telegram client method to the interface first, then the
   implementation, then the fake stub in `*_test.go`.
4. Wire into the orchestrator loop last.
5. Write tests before declaring done.

## CI/CD Feedback Loop

When `ci.enabled: true`, Madar polls the GitHub Actions check suite for the
`madar/issue-N` branch after Claude opens a PR. This is why the branch naming
convention above is mandatory — the CI watcher looks for that exact branch.

## Common Pitfalls

- `BuildFirstRunPrompt` takes `issueNumber int` — don't forget it when adding
  new call sites.
- `escapeMarkdown` must escape `_ * \` [ ` — all four MarkdownV1 specials.
- Telegram messages are capped at 4096 bytes in `send()`.
- `gitEnvWithToken` injects credentials via environment; never pass a token
  in a git URL argument or config file.
- `ci.wait_timeout` is checked in `advanceCITask` against `task.UpdatedAt` —
  make sure `UpdatedAt` is refreshed when CI state changes.
