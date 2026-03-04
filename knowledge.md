# Project knowledge

## What is this?
VibeProxy is a native macOS menu bar app (Swift/SwiftUI) that proxies AI coding tool requests through users' existing Claude, ChatGPT, Gemini, Qwen, Antigravity, and Z.AI subscriptions. Built on CLIProxyAPIPlus for OAuth handling, token management, and API routing. Targets macOS 14.0+ (Sonoma).

## Key directories
- `src/Sources/` — All Swift source files (AppDelegate, ServerManager, SettingsView, ThinkingProxy, etc.)
- `src/Sources/Resources/` — Bundled assets: CLIProxyAPIPlus binary, config.yaml, app icons
- `src/Package.swift` — Swift Package Manager config (dependencies: Sparkle for auto-updates)
- `.github/workflows/` — CI: auto-release, CLIProxyAPIPlus bump workflow
- `scripts/` — Release helper scripts

## Commands
- Build (debug): `make build` (runs `cd src && swift build`)
- Build (release): `make release` (runs `./build.sh`)
- Create .app bundle: `make app` (runs `./create-app-bundle.sh`)
- Install to /Applications: `make install`
- Run: `make run`
- Clean: `make clean`
- Test build: `make test` (just verifies the build compiles)

## Architecture
- **AppDelegate** — Menu bar item, window lifecycle
- **ServerManager** — Controls the CLIProxyAPIPlus server process, OAuth auth flows
- **ThinkingProxy** — Intercepts requests on port 8317, adds extended thinking params, forwards to CLIProxyAPI on port 8318
- **TunnelManager** — Manages Vercel AI Gateway tunnels
- **SettingsView** — SwiftUI settings UI with provider connect/disconnect, provider priority toggles
- **AuthStatus** — Monitors `~/.cli-proxy-api/` for credential files, real-time file system watching
- **IconCatalog** — Thread-safe icon caching singleton

## Data flow
```
Client (Factory/Amp CLI) → ThinkingProxy (:8317) → CLIProxyAPIPlus (:8318) → Provider APIs
```
ThinkingProxy strips `-thinking-N` suffixes from model names, injects thinking params & interleaved thinking headers, then forwards to CLIProxyAPIPlus which handles OAuth tokens and API routing.

## Conventions
- Swift 5.9+, SwiftUI for UI, AppKit for menu bar integration
- Logging via `NSLog` throughout
- Notification names centralized in `NotificationNames.swift`
- Auth tokens stored in `~/.cli-proxy-api/` with 0600 permissions
- CLIProxyAPIPlus binary is bundled inside the .app Resources folder
- Versions are auto-injected from git tags during build
- Most releases are automated CLIProxyAPIPlus version bumps via GitHub Actions

## Gotchas
- This is a macOS-only project — cannot build/run on Linux (AppKit/SwiftUI dependencies)
- `LSUIElement` is true in Info.plist (app runs as menu bar agent, no dock icon)
- ThinkingProxy listens on 8317, CLIProxyAPIPlus on 8318 — both localhost only
- The `-config` flag uses single dash (not `--config`)
- Sparkle framework handles auto-updates with EdDSA signed appcast
- Config uses YAML; API keys with special chars need proper escaping
