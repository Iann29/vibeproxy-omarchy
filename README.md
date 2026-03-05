<p align="center">
  <img src="icon.png" width="128" height="128" alt="VibeProxy">
</p>

<h1 align="center">VibeProxy Linux</h1>

<p align="center">
  <strong>Use your AI subscriptions with any coding tool. No API keys needed.</strong>
</p>

<p align="center">
  <a href="https://github.com/automazeio/vibeproxy/blob/main/LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/License-MIT-28a745"></a>
  <img alt="Go 1.22+" src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white">
  <img alt="Linux" src="https://img.shields.io/badge/Platform-Linux-FCC624?logo=linux&logoColor=black">
  <a href="https://github.com/automazeio/vibeproxy"><img alt="Stars" src="https://img.shields.io/github/stars/automazeio/vibeproxy.svg?style=social&label=Star"></a>
</p>

<p align="center">
  <a href="#installation">Install</a> •
  <a href="#quick-start">Quick Start</a> •
  <a href="#cli-reference">CLI Reference</a> •
  <a href="#configuration">Config</a> •
  <a href="#waybar-integration">Waybar</a> •
  <a href="#architecture">Architecture</a>
</p>

---

> **🐧 Linux port** of [VibeProxy](https://github.com/automazeio/vibeproxy) — a lightweight Go CLI that proxies AI coding tool requests through your existing subscriptions. Originally a macOS menu bar app, this fork is rebuilt from scratch as a native Linux CLI with Waybar integration.

## What it does

VibeProxy sits between your AI coding tools (like [Factory](https://factory.ai), [Amp](https://ampcode.com), [Codebuff](https://codebuff.com)) and your existing AI subscriptions — so you don't need separate API keys.

```
┌──────────────┐      ┌─────────────────────┐      ┌──────────────────────┐      ┌──────────────┐
│  Coding Tool │ ──▶  │  ThinkingProxy:8317  │ ──▶  │  CLIProxyAPI+:8318   │ ──▶  │  Provider    │
│  (Factory,   │      │  • thinking params   │      │  • OAuth tokens      │      │  (Claude,    │
│   Amp, etc.) │      │  • model routing     │      │  • token refresh     │      │   Gemini,    │
│              │ ◀──  │  • Codebuff relay    │ ◀──  │  • round-robin       │ ◀──  │   GPT, etc.) │
└──────────────┘      └─────────────────────┘      └──────────────────────┘      └──────────────┘
```

## Features

- 🔐 **OAuth Authentication** — One-command login for Claude, Codex, Copilot, Gemini, Qwen, and Antigravity
- 🧠 **Extended Thinking** — Automatically injects thinking parameters for Claude models
- 🛡️ **Vercel AI Gateway** — Route Claude requests through [Vercel's AI Gateway](https://vercel.com/docs/ai-gateway) for safer access
- 👥 **Multi-Account** — Connect multiple accounts per provider with automatic round-robin and failover
- 🔌 **Codebuff Integration** — Route requests through Codebuff with the `codebuff/` model prefix
- 📊 **Waybar Module** — Native Waybar integration showing proxy status and connected providers
- ⚡ **Zero Config Start** — Works out of the box with sensible defaults
- 🪶 **Lightweight** — Single Go binary, minimal dependencies

## Supported Providers

| Provider | Auth Method | Command |
|----------|------------|---------|
| Claude | OAuth (browser) | `vibeproxy auth claude` |
| Codex | OAuth (browser) | `vibeproxy auth codex` |
| GitHub Copilot | OAuth (browser) | `vibeproxy auth copilot` |
| Gemini | OAuth (browser) | `vibeproxy auth gemini` |
| Qwen | OAuth (browser) | `vibeproxy auth qwen` |
| Antigravity | OAuth (browser) | `vibeproxy auth antigravity` |
| Z.AI GLM | API key | `vibeproxy auth zai <key>` |
| Codebuff | Browser fingerprint | `vibeproxy auth codebuff` |

## Installation

**Requirements:** Linux (amd64 or arm64), Go 1.22+, `curl`, and a web browser (for OAuth)

```bash
# Clone the repo
git clone https://github.com/vibeproxy/vibeproxy-linux.git
cd vibeproxy-linux

# Full setup: downloads the backend binary + builds & installs vibeproxy
make setup
```

This installs `vibeproxy` to `~/.local/bin/` — make sure it's in your `PATH`.

### Manual steps

```bash
make download-binary   # Download CLIProxyAPIPlus to ~/.local/share/vibeproxy/
make build             # Build to .build/vibeproxy
make install           # Copy binary to ~/.local/bin/vibeproxy
```

## Quick Start

```bash
# 1. Authenticate with a provider
vibeproxy auth claude

# 2. Start the proxy
vibeproxy start

# 3. Point your coding tool to http://127.0.0.1:8317
```

That's it. Configure your AI coding tool to use `http://127.0.0.1:8317` as the API base URL.

### Stopping

```bash
# From another terminal
vibeproxy stop
```

## CLI Reference

```
🔌 VibeProxy Linux

USAGE:
  vibeproxy <command> [arguments]

COMMANDS:
  start              Start the proxy (foreground)
  stop               Stop a running proxy
  status             Show proxy status and auth info
  auth <provider>    Authenticate with a provider
  config             Show current configuration
  waybar             Output Waybar-compatible JSON status
  version            Show version
  help               Show help text
```

### `vibeproxy start`

Starts the proxy in the foreground. Runs ThinkingProxy on port **8317** and the backend on port **8318**. Press `Ctrl+C` to stop gracefully.

### `vibeproxy auth <provider>`

Opens the browser for OAuth login. Supported providers:

```bash
vibeproxy auth claude              # Claude (OAuth)
vibeproxy auth codex               # Codex (OAuth)
vibeproxy auth copilot             # GitHub Copilot (OAuth)
vibeproxy auth gemini              # Gemini (OAuth)
vibeproxy auth qwen                # Qwen (OAuth)
vibeproxy auth qwen user@email     # Qwen with email hint
vibeproxy auth antigravity         # Antigravity (OAuth)
vibeproxy auth zai sk-abc123       # Z.AI (API key)
vibeproxy auth codebuff            # Codebuff (browser fingerprint)
```

### `vibeproxy status`

Shows whether the proxy is running, port info, and authentication status for all providers.

### `vibeproxy waybar`

Outputs a JSON object for Waybar's custom module. See [Waybar Integration](#waybar-integration).

## Configuration

Config file: `~/.config/vibeproxy/config.yaml`

A default config is created on first run. Here's a full example:

```yaml
# Proxy port (clients connect here)
proxy_port: 8317

# Backend port (CLIProxyAPIPlus)
backend_port: 8318

# Path to the CLIProxyAPIPlus binary
binary_path: ~/.local/share/vibeproxy/cli-proxy-api-plus

# Auth credentials directory
auth_dir: ~/.cli-proxy-api

# Vercel AI Gateway (optional, for safer Claude access)
vercel_gateway_enabled: false
vercel_api_key: ""

# Enable/disable specific providers
enabled_providers:
  claude: true
  codex: true
  copilot: true
  gemini: true
  qwen: true
  antigravity: true
  zai: true

# Debug logging
debug: false
```

### File Paths

| Path | Purpose |
|------|---------|
| `~/.config/vibeproxy/config.yaml` | User configuration |
| `~/.cli-proxy-api/*.json` | Auth credentials (0600 permissions) |
| `~/.local/share/vibeproxy/` | Backend binary, PID files, generated configs |

## Waybar Integration

Add a custom module to your Waybar config to see VibeProxy status at a glance.

**~/.config/waybar/config.jsonc:**

```jsonc
{
  "modules-right": ["custom/vibeproxy"],
  "custom/vibeproxy": {
    "exec": "vibeproxy waybar",
    "return-type": "json",
    "interval": 10,
    "on-click": "vibeproxy status"
  }
}
```

**~/.config/waybar/style.css:**

```css
#custom-vibeproxy {
  padding: 0 8px;
}

#custom-vibeproxy.stopped {
  color: #f38ba8;
}

#custom-vibeproxy.running {
  color: #a6e3a1;
}
```

The module shows a 🔌 icon with the number of active providers, and a tooltip with full status details.

## Architecture

```
vibeproxy
├── cmd/vibeproxy/main.go        # CLI entry point (start, stop, status, auth, config, waybar)
├── internal/
│   ├── proxy/thinking.go        # HTTP reverse proxy: thinking injection, Codebuff/Vercel routing
│   ├── server/manager.go        # CLIProxyAPIPlus subprocess lifecycle, auth commands
│   ├── auth/manager.go          # Credential management (~/.cli-proxy-api/)
│   ├── config/config.go         # YAML config loading, backend config generation
│   └── notify/notify.go         # Linux desktop helpers (notify-send, xdg-open, clipboard)
├── configs/config.yaml          # Reference backend config
├── scripts/
│   ├── download-binary.sh       # Downloads latest CLIProxyAPIPlus release
│   └── create-release.sh        # Release automation
└── Makefile                     # Build, install, setup targets
```

### Data Flow

1. **Client** sends request to ThinkingProxy (`:8317`)
2. **ThinkingProxy** inspects the model name:
   - `codebuff/*` → routed to Codebuff's API
   - `*-thinking-N` → thinking params injected, forwarded to backend
   - Claude + Vercel enabled → routed through Vercel AI Gateway
   - Everything else → forwarded to CLIProxyAPIPlus
3. **CLIProxyAPIPlus** (`:8318`) handles OAuth token refresh, provider routing, and round-robin across accounts
4. **Response** streams back through the proxy to the client

## Credits

Built on top of [CLIProxyAPIPlus](https://github.com/router-for-me/CLIProxyAPIPlus) for OAuth handling, token management, and API routing. Linux port by the community.

## License

[MIT License](LICENSE)

---

<p align="center">
  <sub>Originally created by <a href="https://automaze.io">Automaze</a> · Linux port with 🐧 by the community</sub>
</p>
