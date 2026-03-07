# Project knowledge

## What is this?
VibeProxy Linux is a Go CLI tool that proxies AI coding tool requests through users' existing Claude, ChatGPT, Gemini, Qwen, Antigravity, Codebuff, and Z.AI subscriptions. It wraps the CLIProxyAPIPlus binary with a ThinkingProxy layer that adds extended thinking support for Claude models. Targets Linux (amd64/arm64).

## Key directories
- `cmd/vibeproxy/main.go` — CLI entry point with all commands (start, stop, restart, status, auth, config, waybar, version)
- `internal/proxy/thinking.go` — HTTP reverse proxy: thinking parameter injection, Vercel/Codebuff/Amp routing, `/v1/models` Codebuff injection
- `internal/server/manager.go` — Manages the CLIProxyAPIPlus subprocess lifecycle, auth commands, log capture
- `internal/auth/manager.go` — Reads/writes auth credential JSON files in `~/.cli-proxy-api/`
- `internal/config/config.go` — Config loading/saving, backend config generation with provider overrides
- `internal/notify/notify.go` — Linux desktop helpers: `notify-send`, `wl-copy`/`xclip`, `xdg-open`
- `configs/config.yaml` — Reference backend config (embedded as Go constant in config.go)
- `scripts/download-binary.sh` — Downloads latest CLIProxyAPIPlus release from GitHub
- `.github/workflows/` — CI: auto-release, CLIProxyAPIPlus bump workflow

## Commands
- Build: `make build` (runs `go build` → `.build/vibeproxy`)
- Install: `make install` (copies binary to `~/.local/bin/vibeproxy`)
- Full setup: `make setup` (downloads CLIProxyAPIPlus binary + installs vibeproxy)
- Download backend binary: `make download-binary`
- Run: `make run` (build + start)
- Clean: `make clean`
- Deps: `make deps` (runs `go mod tidy`)
- No test suite exists yet

## Architecture & Data flow
```
Client (Factory/Amp CLI) → ThinkingProxy (:8317) → CLIProxyAPIPlus (:8318) → Provider APIs
```
- **ThinkingProxy** listens on port 8317, intercepts requests to:
  - Strip `-thinking-N` suffixes from Claude model names and inject thinking parameters
  - Route `codebuff/` prefixed models to Codebuff's API
  - Route Claude requests through Vercel AI Gateway when configured
  - Forward Amp CLI management requests to ampcode.com
  - Forward everything else to CLIProxyAPIPlus on port 8318
- **CLIProxyAPIPlus** (external binary) handles OAuth tokens and API routing to providers

## Config & data paths
- User config: `~/.config/vibeproxy/config.yaml` (YAML, loaded by `internal/config`)
- Auth credentials: `~/.cli-proxy-api/*.json` (one JSON file per account, 0600 permissions)
- Data dir: `~/.local/share/vibeproxy/` (backend binary, PID files, generated configs)
- PID files: `vibeproxy.pid` and `backend.pid` in data dir

## Supported providers
Claude, Codex, GitHub Copilot, Gemini, Qwen, Antigravity, Z.AI GLM, Codebuff

## Conventions
- Go 1.22, single module (`github.com/vibeproxy/vibeproxy-linux`)
- Only external dependency: `gopkg.in/yaml.v3`
- Logging via stdlib `log` package throughout
- ANSI color output in CLI (defined as constants in main.go)
- Auth flow uses the CLIProxyAPIPlus binary's `-<provider>-login` flags
- Z.AI uses API keys (not OAuth); Codebuff supports browser-based fingerprint auth OR direct API key (`cb-pat-...`)
- Version injected via `-ldflags` at build time

## Gotchas
- Linux-only project — uses `notify-send`, `xdg-open`, `wl-copy`/`xclip`, `pgrep`/`pkill`
- ThinkingProxy listens on 8317, CLIProxyAPIPlus on 8318 — both localhost only
- The CLIProxyAPIPlus binary uses single-dash flags (`-config`, `-claude-login`) but the auth command in server/manager.go uses `--config` for the config flag
- Backend config is dynamically generated at startup (merges base config + Z.AI keys + provider exclusions)
- `vibeproxy start` runs in the foreground; use `vibeproxy stop` from another terminal
- `vibeproxy restart` sends SIGTERM to any running instance, then re-execs via `syscall.Exec`
- Waybar integration via `vibeproxy waybar` outputs JSON for a custom Waybar module
