package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vibeproxy/vibeproxy-linux/internal/auth"
	"github.com/vibeproxy/vibeproxy-linux/internal/config"
	"github.com/vibeproxy/vibeproxy-linux/internal/notify"
	"github.com/vibeproxy/vibeproxy-linux/internal/proxy"
	"github.com/vibeproxy/vibeproxy-linux/internal/server"
)

const codebuffAPIBase = "https://www.codebuff.com"
const defaultClaudevibeModel = "claude-opus-4-6"

// providerToAuthCommand maps CLI provider names to server.AuthCommand values.
var providerToAuthCommand = map[string]server.AuthCommand{
	"claude":      server.AuthClaude,
	"codex":       server.AuthCodex,
	"copilot":     server.AuthCopilot,
	"gemini":      server.AuthGemini,
	"qwen":        server.AuthQwen,
	"antigravity": server.AuthAntigravity,
}

var version = "dev"

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorBold   = "\033[1m"
)

func main() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(0)
	}

	command := os.Args[1]

	switch command {
	case "start":
		cmdStart()
	case "stop":
		cmdStop()
	case "restart":
		cmdRestart()
	case "status":
		cmdStatus()
	case "toggle":
		cmdToggle()
	case "auth":
		cmdAuth()
	case "config":
		cmdConfig()
	case "claudevibe":
		cmdClaudevibe()
	case "menu":
		cmdMenu()
	case "waybar":
		cmdWaybar()
	case "version":
		cmdVersion()
	case "help", "--help", "-h":
		printHelp()
	default:
		fmt.Fprintf(os.Stderr, "%sUnknown command: %s%s\n\n", colorRed, command, colorReset)
		printHelp()
		os.Exit(1)
	}
}

// cmdStart starts the proxy in the foreground with graceful shutdown.
func cmdStart() {
	// 1. Load config
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// 2. Ensure directories exist
	if err := cfg.EnsureDirectories(); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to create directories: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// 3. Check binary exists
	if _, err := os.Stat(cfg.BinaryPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "%s✗ Backend binary not found: %s%s\n", colorRed, cfg.BinaryPath, colorReset)
		fmt.Fprintf(os.Stderr, "%s  Run the install script or set binary_path in config.%s\n", colorYellow, colorReset)
		os.Exit(1)
	}

	// 4. Get backend config path
	backendConfigPath, err := cfg.GetBackendConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to get backend config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// Clean up any orphaned backend left behind by a previous crashed proxy.
	if err := stopLingeringBackend(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to stop lingering backend: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// 5. Create server.Manager and start it
	mgr := server.NewManager(cfg.BinaryPath, backendConfigPath, cfg.BackendPort)
	if err := mgr.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to start backend server: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// 6. Create proxy.ThinkingProxy
	tp := &proxy.ThinkingProxy{
		ProxyPort:   cfg.ProxyPort,
		BackendPort: cfg.BackendPort,
	}
	if cfg.VercelGatewayEnabled && cfg.VercelAPIKey != "" {
		tp.VercelConfig = proxy.VercelGatewayConfig{
			Enabled: true,
			APIKey:  cfg.VercelAPIKey,
		}
	}

	// Load Codebuff token if authenticated.
	am := auth.NewAuthManager(cfg.AuthDir)
	if cbToken := am.GetCodebuffToken(); cbToken != "" {
		tp.CodebuffConfig = proxy.CodebuffConfig{Token: cbToken}
	}

	// 7. Start the ThinkingProxy
	if err := tp.Start(); err != nil {
		if stopErr := mgr.Stop(); stopErr != nil {
			fmt.Fprintf(os.Stderr, "%s⚠ Error stopping backend: %v%s\n", colorYellow, stopErr, colorReset)
		}
		fmt.Fprintf(os.Stderr, "%s✗ Failed to start proxy: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// Write PID file
	writePidFile()

	// 8. Print status info
	printBanner(cfg)

	notify.Send("VibeProxy Started", fmt.Sprintf("Proxy running on port %d", cfg.ProxyPort))

	// 9. Wait for SIGINT/SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigChan
	fmt.Printf("\n%s⏹  Received %s, shutting down...%s\n", colorYellow, sig, colorReset)

	// 10. On signal: stop ThinkingProxy, stop ServerManager, exit
	tp.Stop()
	if err := mgr.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "%s⚠ Error stopping backend: %v%s\n", colorYellow, err, colorReset)
	}
	removePidFile()

	fmt.Printf("%s✓ VibeProxy stopped gracefully.%s\n", colorGreen, colorReset)
}

// cmdStop stops a running proxy by sending SIGTERM to the PID from the PID file.
func cmdStop() {
	cfg := loadConfigOrDefault()
	process, pid, source, err := findManagedProcess(config.PidFilePath(), cfg.ProxyPort, "vibeproxy")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ VibeProxy is not running.%s\n", colorRed, colorReset)
		os.Exit(1)
	}

	if source == "port" {
		fmt.Printf("%s⚠ Recovered proxy process from port %d (PID %d) because the PID file was missing or stale.%s\n", colorYellow, cfg.ProxyPort, pid, colorReset)
	}

	if err := stopProcess(pid, process, config.PidFilePath(), "VibeProxy"); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to stop VibeProxy: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	if err := stopLingeringBackend(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to stop backend server: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
}

// cmdRestart stops a running proxy (if any) and re-execs as vibeproxy start.
func cmdRestart() {
	fmt.Printf("%s🔄 Restarting VibeProxy...%s\n", colorBlue, colorReset)

	cfg := loadConfigOrDefault()
	if process, pid, source, err := findManagedProcess(config.PidFilePath(), cfg.ProxyPort, "vibeproxy"); err == nil {
		if source == "port" {
			fmt.Printf("%s⚠ Recovered proxy process from port %d (PID %d) because the PID file was missing or stale.%s\n", colorYellow, cfg.ProxyPort, pid, colorReset)
		}
		if err := stopProcess(pid, process, config.PidFilePath(), "VibeProxy"); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to stop running instance: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
	}

	if err := stopLingeringBackend(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to stop backend server: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to find executable path: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	if err := syscall.Exec(executable, []string{"vibeproxy", "start"}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to restart: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
}

type managedProcessState struct {
	PID    int
	Source string
}

type runtimeState struct {
	Proxy   managedProcessState
	Backend managedProcessState
}

func loadConfigOrDefault() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		return config.DefaultConfig()
	}
	return cfg
}

func inspectRuntimeState(cfg *config.Config) runtimeState {
	state := runtimeState{}
	if _, pid, source, err := findManagedProcess(config.PidFilePath(), cfg.ProxyPort, "vibeproxy"); err == nil {
		state.Proxy = managedProcessState{PID: pid, Source: source}
	}
	if _, pid, source, err := findManagedProcess(config.BackendPidFilePath(), cfg.BackendPort, filepath.Base(cfg.BinaryPath)); err == nil {
		state.Backend = managedProcessState{PID: pid, Source: source}
	}
	return state
}

func stopLingeringBackend(cfg *config.Config) error {
	process, pid, source, err := findManagedProcess(config.BackendPidFilePath(), cfg.BackendPort, filepath.Base(cfg.BinaryPath))
	if err != nil {
		return nil
	}

	if source == "port" {
		fmt.Printf("%s⚠ Recovered backend process from port %d (PID %d).%s\n", colorYellow, cfg.BackendPort, pid, colorReset)
	}

	return stopProcess(pid, process, config.BackendPidFilePath(), "backend server")
}

func stopProcess(pid int, _ *os.Process, pidPath, label string) error {
	fmt.Printf("%s⏹  Sending SIGTERM to %s (PID %d)...%s\n", colorYellow, label, pid, colorReset)

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		os.Remove(pidPath)
		return err
	}

	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if !processExists(pid) {
			fmt.Printf("%s✓ %s stopped (PID %d).%s\n", colorGreen, label, pid, colorReset)
			os.Remove(pidPath)
			return nil
		}
	}

	fmt.Fprintf(os.Stderr, "%s⚠ Process %d did not exit after SIGTERM. Sending SIGKILL...%s\n", colorYellow, pid, colorReset)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		os.Remove(pidPath)
		return err
	}

	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		if !processExists(pid) {
			fmt.Printf("%s✓ %s stopped (PID %d).%s\n", colorGreen, label, pid, colorReset)
			os.Remove(pidPath)
			return nil
		}
	}

	os.Remove(pidPath)
	return fmt.Errorf("process %d is still running", pid)
}

func readLivePID(pidPath string) (int, bool) {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, false
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}

	if !processExists(pid) {
		return 0, false
	}

	return pid, true
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func findManagedProcess(pidPath string, port int, expectedName string) (*os.Process, int, string, error) {
	if pid, ok := readLivePID(pidPath); ok {
		process, err := os.FindProcess(pid)
		if err == nil {
			return process, pid, "pidfile", nil
		}
	}

	pid, err := findListeningPIDByPort(port, expectedName)
	if err != nil {
		return nil, 0, "", err
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, 0, "", err
	}
	return process, pid, "port", nil
}

func findListeningPIDByPort(port int, expectedName string) (int, error) {
	inodes, err := listeningSocketInodes(port)
	if err != nil {
		return 0, err
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}

	var foundPortOccupant bool
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if !processOwnsSocketInode(pid, inodes) {
			continue
		}
		foundPortOccupant = true
		if expectedName == "" || processLooksLike(pid, expectedName) {
			return pid, nil
		}
	}

	if foundPortOccupant {
		return 0, fmt.Errorf("port %d is occupied by a different process", port)
	}

	return 0, os.ErrNotExist
}

func listeningSocketInodes(port int) (map[string]struct{}, error) {
	inodes := make(map[string]struct{})
	for _, procNetPath := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		found, err := socketInodesForPort(procNetPath, port)
		if err != nil {
			return nil, err
		}
		for inode := range found {
			inodes[inode] = struct{}{}
		}
	}

	if len(inodes) == 0 {
		return nil, os.ErrNotExist
	}

	return inodes, nil
}

func socketInodesForPort(path string, port int) (map[string]struct{}, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	defer file.Close()

	return socketInodesForPortFromProcNet(file, port)
}

func socketInodesForPortFromProcNet(r io.Reader, port int) (map[string]struct{}, error) {
	inodes := make(map[string]struct{})
	scanner := bufio.NewScanner(r)
	firstLine := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if firstLine {
			firstLine = false
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}

		localAddr := strings.Split(fields[1], ":")
		if len(localAddr) != 2 {
			continue
		}

		listeningPort, err := strconv.ParseInt(localAddr[1], 16, 32)
		if err != nil || int(listeningPort) != port {
			continue
		}

		inodes[fields[9]] = struct{}{}
	}

	return inodes, scanner.Err()
}

func processOwnsSocketInode(pid int, inodes map[string]struct{}) bool {
	fdPath := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdPath)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(fdPath, entry.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") {
			continue
		}
		inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
		if _, ok := inodes[inode]; ok {
			return true
		}
	}

	return false
}

func processLooksLike(pid int, expectedName string) bool {
	exePath, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err == nil {
		exeBase := filepath.Base(strings.TrimSuffix(exePath, " (deleted)"))
		if strings.Contains(exeBase, expectedName) || strings.Contains(exePath, expectedName) {
			return true
		}
	}

	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err == nil && strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), expectedName) {
		return true
	}

	return false
}

func probePort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// cmdStatus shows whether the proxy is running, port info, and auth status.
func cmdStatus() {
	fmt.Printf("%s%s🔌 VibeProxy Status%s\n", colorBold, colorBlue, colorReset)
	fmt.Println(strings.Repeat("─", 40))

	// Load config and show port info
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s  ✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		return
	}
	state := inspectRuntimeState(cfg)

	if state.Proxy.PID > 0 {
		if state.Proxy.Source == "port" {
			fmt.Printf("  Status:  %s● Running%s (PID %d, recovered from port)\n", colorGreen, colorReset, state.Proxy.PID)
		} else {
			fmt.Printf("  Status:  %s● Running%s (PID %d)\n", colorGreen, colorReset, state.Proxy.PID)
		}
	} else {
		fmt.Printf("  Status:  %s● Stopped%s\n", colorRed, colorReset)
	}

	fmt.Printf("  Proxy:   %shttp://127.0.0.1:%d%s\n", colorBlue, cfg.ProxyPort, colorReset)
	if probePort(cfg.BackendPort) {
		if state.Backend.PID > 0 {
			if state.Backend.Source == "port" {
				fmt.Printf("  Backend: %shttp://127.0.0.1:%d%s %s● up%s (PID %d, recovered)\n", colorBlue, cfg.BackendPort, colorReset, colorGreen, colorReset, state.Backend.PID)
			} else {
				fmt.Printf("  Backend: %shttp://127.0.0.1:%d%s %s● up%s (PID %d)\n", colorBlue, cfg.BackendPort, colorReset, colorGreen, colorReset, state.Backend.PID)
			}
		} else {
			fmt.Printf("  Backend: %shttp://127.0.0.1:%d%s %s● up%s\n", colorBlue, cfg.BackendPort, colorReset, colorGreen, colorReset)
		}
	} else {
		fmt.Printf("  Backend: %shttp://127.0.0.1:%d%s %s● down%s\n", colorBlue, cfg.BackendPort, colorReset, colorRed, colorReset)
	}

	if cfg.VercelGatewayEnabled {
		fmt.Printf("  Vercel:  %s● Enabled%s\n", colorGreen, colorReset)
	}

	// Show auth status
	fmt.Println()
	fmt.Printf("%s%s  Auth Accounts:%s\n", colorBold, colorBlue, colorReset)
	fmt.Println("  " + strings.Repeat("─", 38))

	am := auth.NewAuthManager(cfg.AuthDir)
	status := am.CheckAuthStatus()

	// Define display order
	providers := []auth.ServiceType{
		auth.ServiceClaude,
		auth.ServiceCodex,
		auth.ServiceCopilot,
		auth.ServiceGemini,
		auth.ServiceQwen,
		auth.ServiceAntigravity,
		auth.ServiceZai,
		auth.ServiceCodebuff,
	}

	for _, provider := range providers {
		accounts := status[provider]
		name := provider.DisplayName()

		if len(accounts) == 0 {
			fmt.Printf("  %s✗%s %-18s %snot authenticated%s\n", colorRed, colorReset, name, colorRed, colorReset)
			continue
		}

		for i, acct := range accounts {
			displayProvider := name
			if i > 0 {
				displayProvider = ""
			}
			if acct.IsExpired() {
				fmt.Printf("  %s⚠%s %-18s %s%s (expired)%s\n", colorYellow, colorReset, displayProvider, colorYellow, acct.DisplayName(), colorReset)
			} else {
				fmt.Printf("  %s✓%s %-18s %s%s%s\n", colorGreen, colorReset, displayProvider, colorGreen, acct.DisplayName(), colorReset)
			}
		}
	}

	fmt.Println()
}

// cmdAuth handles authentication subcommands.
func cmdAuth() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "%sUsage:%s\n", colorBold, colorReset)
		fmt.Fprintf(os.Stderr, "  vibeproxy auth <provider>       Authenticate with a provider\n")
		fmt.Fprintf(os.Stderr, "  vibeproxy auth zai <api-key>    Save Z.AI API key\n")
		fmt.Fprintf(os.Stderr, "  vibeproxy auth codebuff         Login to Codebuff via browser\n")
		fmt.Fprintf(os.Stderr, "  vibeproxy auth codebuff <key>   Save Codebuff API key (cb-pat-...)\n")
		fmt.Fprintf(os.Stderr, "\n%sProviders:%s claude, codex, copilot, gemini, qwen, antigravity, codebuff\n", colorBold, colorReset)
		os.Exit(1)
	}

	provider := os.Args[2]

	// Handle "zai" provider separately - it uses an API key
	if provider == "zai" {
		cmdAuthZai()
		return
	}

	// Handle "codebuff" provider separately - uses browser-based fingerprint auth
	if provider == "codebuff" {
		cmdAuthCodebuff()
		return
	}

	// Validate provider name
	authCmd, ok := providerToAuthCommand[provider]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s✗ Unknown provider: %s%s\n", colorRed, provider, colorReset)
		fmt.Fprintf(os.Stderr, "%sValid providers:%s claude, codex, copilot, gemini, qwen, antigravity, zai, codebuff\n", colorBold, colorReset)
		os.Exit(1)
	}

	// Load config to get binary path and backend config
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	if err := cfg.EnsureDirectories(); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to create directories: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	if _, err := os.Stat(cfg.BinaryPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "%s✗ Backend binary not found: %s%s\n", colorRed, cfg.BinaryPath, colorReset)
		os.Exit(1)
	}

	backendConfigPath, err := cfg.GetBackendConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to get backend config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	fmt.Printf("%s🔑 Authenticating with %s...%s\n", colorBlue, provider, colorReset)
	fmt.Printf("%s   Complete login in your browser. (Ctrl+C to cancel)%s\n", colorYellow, colorReset)

	mgr := server.NewManager(cfg.BinaryPath, backendConfigPath, cfg.BackendPort)
	req := server.AuthRequest{Command: authCmd}
	if authCmd == server.AuthQwen && len(os.Args) > 3 {
		req.Email = os.Args[3]
	}

	sp := newSpinner("Waiting for browser authentication...")
	sp.Start()
	msg, err := mgr.RunAuthCommand(req)
	sp.Stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Authentication failed: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	if msg != "" {
		fmt.Println(msg)
	} else {
		fmt.Printf("%s✓ Successfully authenticated with %s.%s\n", colorGreen, provider, colorReset)
	}
}

// cmdAuthZai saves a Z.AI API key.
func cmdAuthZai() {
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "%sUsage:%s vibeproxy auth zai <api-key>\n", colorBold, colorReset)
		os.Exit(1)
	}

	apiKey := os.Args[3]
	if strings.TrimSpace(apiKey) == "" {
		fmt.Fprintf(os.Stderr, "%s✗ API key cannot be empty.%s\n", colorRed, colorReset)
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	am := auth.NewAuthManager(cfg.AuthDir)
	if err := am.SaveZaiAPIKey(apiKey); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to save Z.AI API key: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	fmt.Printf("%s✓ Z.AI API key saved successfully.%s\n", colorGreen, colorReset)
}

// cmdAuthCodebuff implements the Codebuff browser-based fingerprint auth flow.
func cmdAuthCodebuff() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	if err := cfg.EnsureDirectories(); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to create directories: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// If API key is provided directly, skip browser flow.
	if len(os.Args) > 3 {
		apiKey := strings.TrimSpace(os.Args[3])
		apiKey = strings.TrimPrefix(apiKey, "__Secure-next-auth.session-token=")

		if apiKey == "" {
			fmt.Fprintf(os.Stderr, "%s✗ API key cannot be empty.%s\n", colorRed, colorReset)
			os.Exit(1)
		}

		fmt.Printf("%s🔑 Validating Codebuff API key...%s\n", colorBlue, colorReset)

		client := &http.Client{Timeout: 15 * time.Second}
		valReq, err := http.NewRequest("GET", codebuffAPIBase+"/api/user/usage", nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to create request: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		valReq.Header.Set("Cookie", "__Secure-next-auth.session-token="+apiKey)

		valResp, err := client.Do(valReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to connect to Codebuff: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		valResp.Body.Close()

		if valResp.StatusCode != 200 {
			fmt.Fprintf(os.Stderr, "%s✗ Invalid API key (status %d).%s\n", colorRed, valResp.StatusCode, colorReset)
			os.Exit(1)
		}

		am := auth.NewAuthManager(cfg.AuthDir)
		if err := am.SaveCodebuffAPIKey(apiKey); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to save API key: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}

		fmt.Printf("%s✓ Codebuff API key saved successfully.%s\n", colorGreen, colorReset)
		fmt.Printf("  Use model prefix %scodebuff/%s to route requests through Codebuff.\n", colorBlue, colorReset)
		return
	}

	fingerprintID := "vibeproxy-" + config.GenerateRandomHex(8)

	fmt.Printf("%s🔑 Authenticating with Codebuff...%s\n", colorBlue, colorReset)

	// Step 1: Request login code from Codebuff API.
	reqBody, _ := json.Marshal(map[string]string{"fingerprintId": fingerprintID})
	resp, err := http.Post(codebuffAPIBase+"/api/auth/cli/code", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to connect to Codebuff: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "%s✗ Codebuff returned status %d: %s%s\n", colorRed, resp.StatusCode, string(body), colorReset)
		os.Exit(1)
	}

	var loginResp struct {
		LoginURL        string `json:"loginUrl"`
		FingerprintHash string `json:"fingerprintHash"`
		ExpiresAt       int64  `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to parse Codebuff response: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// Step 2: Open browser.
	fmt.Printf("   %sOpening browser...%s\n", colorYellow, colorReset)
	if err := exec.Command("xdg-open", loginResp.LoginURL).Start(); err != nil {
		fmt.Printf("   %s⚠ Could not open browser. Open this URL manually:%s\n", colorYellow, colorReset)
		fmt.Printf("   %s%s%s\n", colorBlue, loginResp.LoginURL, colorReset)
	}
	fmt.Printf("   %sComplete login in your browser. (Ctrl+C to cancel)%s\n", colorYellow, colorReset)

	// Step 3: Poll for authentication status.
	sp := newSpinner("Waiting for browser authentication...")
	sp.Start()

	client := &http.Client{Timeout: 15 * time.Second}
	expiresAtStr := strconv.FormatInt(loginResp.ExpiresAt, 10)
	deadline := time.Now().Add(5 * time.Minute)

	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)

		statusURL := fmt.Sprintf("%s/api/auth/cli/status?fingerprintId=%s&fingerprintHash=%s&expiresAt=%s",
			codebuffAPIBase,
			url.QueryEscape(fingerprintID),
			url.QueryEscape(loginResp.FingerprintHash),
			url.QueryEscape(expiresAtStr),
		)

		statusResp, err := client.Get(statusURL)
		if err != nil {
			continue
		}

		if statusResp.StatusCode == 200 {
			var result struct {
				User struct {
					ID              string `json:"id"`
					Name            string `json:"name"`
					Email           string `json:"email"`
					AuthToken       string `json:"authToken"`
					FingerprintID   string `json:"fingerprintId"`
					FingerprintHash string `json:"fingerprintHash"`
				} `json:"user"`
			}
			if err := json.NewDecoder(statusResp.Body).Decode(&result); err == nil && result.User.AuthToken != "" {
				statusResp.Body.Close()
				sp.Stop()

				am := auth.NewAuthManager(cfg.AuthDir)
				if err := am.SaveCodebuffCredentials(
					result.User.Email,
					result.User.Name,
					result.User.AuthToken,
					result.User.ID,
					fingerprintID,
					loginResp.FingerprintHash,
				); err != nil {
					fmt.Fprintf(os.Stderr, "%s✗ Failed to save credentials: %v%s\n", colorRed, err, colorReset)
					os.Exit(1)
				}

				fmt.Printf("%s✓ Logged in as %s (%s)%s\n", colorGreen, result.User.Name, result.User.Email, colorReset)
				fmt.Printf("  Use model prefix %scodebuff/%s to route requests through Codebuff.\n", colorBlue, colorReset)
				return
			}
		}
		statusResp.Body.Close()
	}

	sp.Stop()
	fmt.Fprintf(os.Stderr, "%s✗ Authentication timed out after 5 minutes.%s\n", colorRed, colorReset)
	os.Exit(1)
}

// cmdClaudevibe launches Claude Code CLI pre-configured to use VibeProxy.
func cmdClaudevibe() {
	// 1. Check if the proxy is running
	running, _ := isProxyRunning()
	if !running {
		fmt.Fprintf(os.Stderr, "%s✗ VibeProxy is not running.%s\n", colorRed, colorReset)
		fmt.Fprintf(os.Stderr, "%s  Start it first with: vibeproxy start%s\n", colorYellow, colorReset)
		os.Exit(1)
	}

	// 2. Load config to get proxy port
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	// 3. Parse --model flag and collect remaining args for claude
	var modelOverride string
	var claudeArgs []string
	rawArgs := os.Args[2:] // everything after "claudevibe"
	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		if (arg == "--model" || arg == "-m") && i+1 < len(rawArgs) {
			modelOverride = rawArgs[i+1]
			i++ // skip the value
		} else if arg == "--model" || arg == "-m" {
			fmt.Fprintf(os.Stderr, "%s✗ %s requires a model name.%s\n", colorRed, arg, colorReset)
			os.Exit(1)
		} else if strings.HasPrefix(arg, "--model=") {
			modelOverride = strings.TrimPrefix(arg, "--model=")
		} else if strings.HasPrefix(arg, "-m=") {
			modelOverride = strings.TrimPrefix(arg, "-m=")
		} else {
			claudeArgs = append(claudeArgs, arg)
		}
	}

	// 4. ClaudeVibe always uses Codebuff-backed Claude models.
	am := auth.NewAuthManager(cfg.AuthDir)
	if am.GetCodebuffToken() == "" {
		fmt.Fprintf(os.Stderr, "%s✗ Not authenticated with Codebuff.%s\n", colorRed, colorReset)
		fmt.Fprintf(os.Stderr, "%s  Run: vibeproxy auth codebuff%s\n", colorYellow, colorReset)
		os.Exit(1)
	}

	claudeModel, err := normalizeClaudevibeModel(modelOverride)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ %v%s\n", colorRed, err, colorReset)
		fmt.Fprintf(os.Stderr, "%s  Example: claudevibe -m claude-opus-4-6%s\n", colorYellow, colorReset)
		fmt.Fprintf(os.Stderr, "%s  Example: claudevibe -m codebuff/anthropic/claude-opus-4-6%s\n", colorYellow, colorReset)
		os.Exit(1)
	}

	// 5. Find the claude binary
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Claude Code CLI not found.%s\n", colorRed, colorReset)
		fmt.Fprintf(os.Stderr, "%s  Install it from: https://docs.anthropic.com/en/docs/claude-code%s\n", colorYellow, colorReset)
		os.Exit(1)
	}

	// 6. Build environment with proxy settings
	env := os.Environ()
	env = setEnvVar(env, "ANTHROPIC_BASE_URL", fmt.Sprintf("http://127.0.0.1:%d/cb", cfg.ProxyPort))
	env = setEnvVar(env, "ANTHROPIC_AUTH_TOKEN", "vibeproxy")
	env = setEnvVar(env, "ANTHROPIC_MODEL", "")

	// 7. Pass --model to claude CLI args (not via env var, for better compatibility)
	claudeArgs = append([]string{"--model", claudeModel}, claudeArgs...)

	// 8. Print banner
	fmt.Printf("%s%s🔌 ClaudeVibe%s\n", colorBold, colorBlue, colorReset)
	fmt.Printf("   Proxy: %shttp://127.0.0.1:%d%s\n", colorGreen, cfg.ProxyPort, colorReset)
	fmt.Printf("   Model: %s%s%s %s(via Codebuff)%s\n", colorGreen, claudeModel, colorReset, colorYellow, colorReset)
	fmt.Println()

	// 9. Exec into claude (replaces this process)
	execArgs := append([]string{"claude"}, claudeArgs...)
	if err := syscall.Exec(claudePath, execArgs, env); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to launch Claude Code: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
}

func normalizeClaudevibeModel(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return defaultClaudevibeModel, nil
	}

	model = strings.TrimPrefix(model, "codebuff/")
	model = strings.TrimPrefix(model, "anthropic/")
	model = normalizeLegacyClaudeModel(model)
	if model == "" {
		return "", fmt.Errorf("invalid Codebuff model")
	}
	if !strings.HasPrefix(model, "claude-") {
		return "", fmt.Errorf("claudevibe only supports Claude models, got %q", model)
	}

	return model, nil
}

func normalizeLegacyClaudeModel(model string) string {
	switch model {
	case "claude-opus-4.6":
		return "claude-opus-4-6"
	case "claude-sonnet-4.6":
		return "claude-sonnet-4-6"
	default:
		return model
	}
}

// setEnvVar sets or replaces an environment variable in an env slice.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// cmdConfig shows the current configuration.
func cmdConfig() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to load config: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	fmt.Printf("%s%s⚙  VibeProxy Configuration%s\n", colorBold, colorBlue, colorReset)
	fmt.Println(strings.Repeat("─", 40))
	fmt.Printf("  Proxy Port:     %s%d%s\n", colorBlue, cfg.ProxyPort, colorReset)
	fmt.Printf("  Backend Port:   %s%d%s\n", colorBlue, cfg.BackendPort, colorReset)
	fmt.Printf("  Binary Path:    %s\n", cfg.BinaryPath)
	fmt.Printf("  Auth Dir:       %s\n", cfg.AuthDir)
	fmt.Printf("  Debug:          %v\n", cfg.Debug)

	if cfg.VercelGatewayEnabled {
		fmt.Printf("  Vercel Gateway: %s● Enabled%s\n", colorGreen, colorReset)
		if cfg.VercelAPIKey != "" {
			masked := "****"
			if len(cfg.VercelAPIKey) > 12 {
				masked = cfg.VercelAPIKey[:4] + "..." + cfg.VercelAPIKey[len(cfg.VercelAPIKey)-4:]
			}
			fmt.Printf("  Vercel API Key: %s\n", masked)
		}
	} else {
		fmt.Printf("  Vercel Gateway: %s● Disabled%s\n", colorRed, colorReset)
	}

	if len(cfg.EnabledProviders) > 0 {
		fmt.Println()
		fmt.Printf("  %sProvider Overrides:%s\n", colorBold, colorReset)
		for provider, enabled := range cfg.EnabledProviders {
			if enabled {
				fmt.Printf("    %s✓%s %s\n", colorGreen, colorReset, provider)
			} else {
				fmt.Printf("    %s✗%s %s (disabled)\n", colorRed, colorReset, provider)
			}
		}
	}

	fmt.Println()
}

// cmdWaybar outputs Waybar-compatible JSON for the custom module.
func cmdWaybar() {
	type waybarOutput struct {
		Text    string `json:"text"`
		Tooltip string `json:"tooltip"`
		Class   string `json:"class"`
		Alt     string `json:"alt"`
	}

	cfg := loadConfigOrDefault()
	state := inspectRuntimeState(cfg)
	backendUp := probePort(cfg.BackendPort)

	am := auth.NewAuthManager(cfg.AuthDir)
	status := am.CheckAuthStatus()

	providers := []auth.ServiceType{
		auth.ServiceClaude, auth.ServiceCodex, auth.ServiceCopilot,
		auth.ServiceGemini, auth.ServiceQwen, auth.ServiceAntigravity,
		auth.ServiceZai, auth.ServiceCodebuff,
	}

	activeCount := 0
	var lines []string
	for _, provider := range providers {
		accounts := status[provider]
		name := provider.DisplayName()
		if len(accounts) == 0 {
			lines = append(lines, fmt.Sprintf("✗ %s", name))
		} else {
			hasActive := false
			for _, acct := range accounts {
				if acct.IsExpired() {
					lines = append(lines, fmt.Sprintf("⚠ %s (%s, expired)", name, acct.DisplayName()))
				} else {
					lines = append(lines, fmt.Sprintf("✓ %s (%s)", name, acct.DisplayName()))
					hasActive = true
				}
			}
			if hasActive {
				activeCount++
			}
		}
	}

	proxyStatus := "down"
	if state.Proxy.PID > 0 {
		if state.Proxy.Source == "port" {
			proxyStatus = fmt.Sprintf("up (PID %d, recovered)", state.Proxy.PID)
		} else {
			proxyStatus = fmt.Sprintf("up (PID %d)", state.Proxy.PID)
		}
	}

	backendStatus := "down"
	if backendUp {
		if state.Backend.PID > 0 {
			if state.Backend.Source == "port" {
				backendStatus = fmt.Sprintf("up (PID %d, recovered)", state.Backend.PID)
			} else {
				backendStatus = fmt.Sprintf("up (PID %d)", state.Backend.PID)
			}
		} else {
			backendStatus = "up"
		}
	}

	moduleClass := "stopped"
	moduleAlt := "stopped"
	moduleText := "🔌 off"
	moduleTitle := "VibeProxy ● Stopped"

	switch {
	case state.Proxy.PID > 0 && backendUp:
		moduleClass = "running"
		moduleAlt = "running"
		moduleText = fmt.Sprintf("🔌 %d", activeCount)
		moduleTitle = "VibeProxy ● Running"
	case state.Proxy.PID > 0 && !backendUp:
		moduleClass = "degraded"
		moduleAlt = "degraded"
		moduleText = "🔌 !"
		moduleTitle = "VibeProxy ● Degraded"
	}

	tooltip := fmt.Sprintf("%s\nProxy: localhost:%d %s\nBackend: localhost:%d %s\n\n%s\n\nActions:\nLeft click: menu\nRight click: toggle\nMiddle click: restart",
		moduleTitle, cfg.ProxyPort, proxyStatus, cfg.BackendPort, backendStatus, strings.Join(lines, "\n"))

	out := waybarOutput{
		Text:    moduleText,
		Tooltip: tooltip,
		Class:   moduleClass,
		Alt:     moduleAlt,
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

// cmdVersion prints the version string.
func cmdVersion() {
	fmt.Printf("VibeProxy Linux %s%s%s\n", colorBold, version, colorReset)
}

func cmdToggle() {
	cfg := loadConfigOrDefault()
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to find executable path: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	state := inspectRuntimeState(cfg)
	if state.Proxy.PID > 0 {
		process, pid, source, err := findManagedProcess(config.PidFilePath(), cfg.ProxyPort, "vibeproxy")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to locate running proxy: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		if source == "port" {
			fmt.Printf("%s⚠ Recovered proxy process from port %d (PID %d).%s\n", colorYellow, cfg.ProxyPort, pid, colorReset)
		}
		if err := stopProcess(pid, process, config.PidFilePath(), "VibeProxy"); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to stop VibeProxy: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		if err := stopLingeringBackend(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to stop backend server: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		notify.Send("VibeProxy", "Proxy stopped")
		return
	}

	if err := startProxyDetached(executable); err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to start VibeProxy in background: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}
	if waitForPortState(cfg.ProxyPort, true, 5*time.Second) {
		fmt.Printf("%s✓ VibeProxy started in background.%s\n", colorGreen, colorReset)
		notify.Send("VibeProxy", fmt.Sprintf("Proxy started on port %d", cfg.ProxyPort))
		return
	}

	fmt.Fprintf(os.Stderr, "%s✗ VibeProxy did not start on port %d. Check %s.%s\n", colorRed, cfg.ProxyPort, actionLogPath(), colorReset)
	notify.Send("VibeProxy", fmt.Sprintf("Failed to start. Check %s", actionLogPath()))
	os.Exit(1)
}

func cmdMenu() {
	cfg := loadConfigOrDefault()
	state := inspectRuntimeState(cfg)

	var options []string
	if state.Proxy.PID > 0 {
		options = append(options, "Restart proxy", "Stop proxy")
	} else {
		options = append(options, "Start proxy")
	}
	options = append(options, "Show status", "Open action log")

	selection, err := runLauncherMenu("VibeProxy", options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to open launcher menu: %v%s\n", colorRed, err, colorReset)
		notify.Send("VibeProxy", err.Error())
		os.Exit(1)
	}
	if selection == "" {
		return
	}

	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s✗ Failed to find executable path: %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	switch selection {
	case "Start proxy":
		if err := startProxyDetached(executable); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to start VibeProxy: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to start: %v", err))
			os.Exit(1)
		}
		if waitForPortState(cfg.ProxyPort, true, 5*time.Second) {
			notify.Send("VibeProxy", fmt.Sprintf("Proxy started on port %d", cfg.ProxyPort))
			return
		}
		notify.Send("VibeProxy", fmt.Sprintf("Start timed out. Check %s", actionLogPath()))
		os.Exit(1)
	case "Stop proxy":
		process, pid, source, err := findManagedProcess(config.PidFilePath(), cfg.ProxyPort, "vibeproxy")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to locate running proxy: %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
		if source == "port" {
			fmt.Printf("%s⚠ Recovered proxy process from port %d (PID %d).%s\n", colorYellow, cfg.ProxyPort, pid, colorReset)
		}
		if err := stopProcess(pid, process, config.PidFilePath(), "VibeProxy"); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to stop VibeProxy: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to stop: %v", err))
			os.Exit(1)
		}
		if err := stopLingeringBackend(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to stop backend server: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to stop backend: %v", err))
			os.Exit(1)
		}
		notify.Send("VibeProxy", "Proxy stopped")
	case "Restart proxy":
		if process, pid, source, err := findManagedProcess(config.PidFilePath(), cfg.ProxyPort, "vibeproxy"); err == nil {
			if source == "port" {
				fmt.Printf("%s⚠ Recovered proxy process from port %d (PID %d).%s\n", colorYellow, cfg.ProxyPort, pid, colorReset)
			}
			if err := stopProcess(pid, process, config.PidFilePath(), "VibeProxy"); err != nil {
				fmt.Fprintf(os.Stderr, "%s✗ Failed to stop VibeProxy: %v%s\n", colorRed, err, colorReset)
				notify.Send("VibeProxy", fmt.Sprintf("Failed to restart: %v", err))
				os.Exit(1)
			}
			waitForPortState(cfg.ProxyPort, false, 3*time.Second)
		}
		if err := stopLingeringBackend(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to stop backend server: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to restart backend: %v", err))
			os.Exit(1)
		}
		if err := startProxyDetached(executable); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to restart VibeProxy: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to restart: %v", err))
			os.Exit(1)
		}
		if waitForPortState(cfg.ProxyPort, true, 5*time.Second) {
			notify.Send("VibeProxy", fmt.Sprintf("Proxy restarted on port %d", cfg.ProxyPort))
			return
		}
		notify.Send("VibeProxy", fmt.Sprintf("Restart timed out. Check %s", actionLogPath()))
		os.Exit(1)
	case "Show status":
		if err := openInTerminal(shellQuote(executable) + " status; printf '\\nPressione Enter para fechar...'; read _"); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to open status terminal: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to open terminal: %v", err))
			os.Exit(1)
		}
	case "Open action log":
		if err := openInTerminal("tail -n 80 -f " + shellQuote(actionLogPath())); err != nil {
			fmt.Fprintf(os.Stderr, "%s✗ Failed to open log terminal: %v%s\n", colorRed, err, colorReset)
			notify.Send("VibeProxy", fmt.Sprintf("Failed to open terminal: %v", err))
			os.Exit(1)
		}
	}
}

func startProxyDetached(executable string) error {
	cfg := loadConfigOrDefault()
	if err := cfg.EnsureDirectories(); err != nil {
		return err
	}

	logFile, err := os.OpenFile(actionLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(executable, "start")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return err
	}

	return nil
}

func waitForPortState(port int, wantUp bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if probePort(port) == wantUp {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return probePort(port) == wantUp
}

func runLauncherMenu(prompt string, options []string) (string, error) {
	cmd, err := launcherMenuCommand(prompt)
	if err != nil {
		return "", err
	}

	cmd.Stdin = strings.NewReader(strings.Join(options, "\n"))
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", nil
		}
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func launcherMenuCommand(prompt string) (*exec.Cmd, error) {
	switch {
	case commandExists("wofi"):
		return exec.Command("wofi", "--show", "dmenu", "--prompt", prompt, "--insensitive"), nil
	case commandExists("rofi"):
		return exec.Command("rofi", "-dmenu", "-i", "-p", prompt), nil
	case commandExists("fuzzel"):
		return exec.Command("fuzzel", "--dmenu", "--prompt", prompt+"> "), nil
	case commandExists("bemenu"):
		return exec.Command("bemenu", "-p", prompt), nil
	case commandExists("dmenu"):
		return exec.Command("dmenu", "-p", prompt), nil
	default:
		return nil, errors.New("no supported launcher found (install wofi, rofi, fuzzel, bemenu, or dmenu)")
	}
}

func openInTerminal(shellCommand string) error {
	switch {
	case commandExists("ghostty"):
		return exec.Command("ghostty", "-e", "sh", "-lc", shellCommand).Start()
	case commandExists("alacritty"):
		return exec.Command("alacritty", "-e", "sh", "-lc", shellCommand).Start()
	case commandExists("kitty"):
		return exec.Command("kitty", "sh", "-lc", shellCommand).Start()
	case commandExists("foot"):
		return exec.Command("foot", "sh", "-lc", shellCommand).Start()
	case commandExists("wezterm"):
		return exec.Command("wezterm", "start", "--", "sh", "-lc", shellCommand).Start()
	case commandExists("xterm"):
		return exec.Command("xterm", "-e", "sh", "-lc", shellCommand).Start()
	default:
		return errors.New("no supported terminal found (ghostty, alacritty, kitty, foot, wezterm, or xterm)")
	}
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func actionLogPath() string {
	return filepath.Join(config.DataDir(), "actions.log")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// printHelp prints the CLI help text.
func printHelp() {
	fmt.Printf("%s%s🔌 VibeProxy Linux%s %s\n\n", colorBold, colorBlue, colorReset, version)
	fmt.Printf("%sUSAGE:%s\n", colorBold, colorReset)
	fmt.Printf("  vibeproxy <command> [arguments]\n\n")
	fmt.Printf("%sCOMMANDS:%s\n", colorBold, colorReset)
	fmt.Printf("  %sstart%s              Start the proxy (foreground)\n", colorGreen, colorReset)
	fmt.Printf("  %sstop%s               Stop a running proxy\n", colorGreen, colorReset)
	fmt.Printf("  %srestart%s            Restart the proxy\n", colorGreen, colorReset)
	fmt.Printf("  %sstatus%s             Show proxy status and auth info\n", colorGreen, colorReset)
	fmt.Printf("  %stoggle%s             Start or stop the proxy in background\n", colorGreen, colorReset)
	fmt.Printf("  %sauth <provider>%s    Authenticate with a provider\n", colorGreen, colorReset)
	fmt.Printf("  %sauth zai <key>%s     Save a Z.AI API key\n", colorGreen, colorReset)
	fmt.Printf("  %sconfig%s             Show current configuration\n", colorGreen, colorReset)
	fmt.Printf("  %sclaudevibe [-m mod]%s Launch Claude Code via VibeProxy\n", colorGreen, colorReset)
	fmt.Printf("  %smenu%s               Open a Waybar-friendly action menu\n", colorGreen, colorReset)
	fmt.Printf("  %swaybar%s             Output Waybar-compatible JSON status\n", colorGreen, colorReset)
	fmt.Printf("  %sversion%s            Show version\n", colorGreen, colorReset)
	fmt.Printf("  %shelp%s               Show this help text\n", colorGreen, colorReset)
	fmt.Println()
	fmt.Printf("%sPROVIDERS:%s\n", colorBold, colorReset)
	fmt.Printf("  claude, codex, copilot, gemini, qwen, antigravity, zai, codebuff\n")
	fmt.Println()
	fmt.Printf("%sEXAMPLES:%s\n", colorBold, colorReset)
	fmt.Printf("  vibeproxy start                 Start proxy in foreground\n")
	fmt.Printf("  vibeproxy auth claude            Authenticate with Claude\n")
	fmt.Printf("  vibeproxy auth qwen user@mail    Authenticate Qwen with email\n")
	fmt.Printf("  vibeproxy auth zai sk-abc123     Save Z.AI API key\n")
	fmt.Printf("  vibeproxy auth codebuff          Login to Codebuff via browser\n")
	fmt.Printf("  vibeproxy auth codebuff <key>    Save Codebuff API key directly\n")
	fmt.Printf("  vibeproxy stop                   Stop the proxy\n")
	fmt.Printf("  vibeproxy restart                Restart the proxy\n")
	fmt.Printf("  vibeproxy toggle                 Start/stop in background\n")
	fmt.Printf("  vibeproxy menu                   Open the launcher menu\n")
	fmt.Printf("  vibeproxy claudevibe             Launch Claude Code via Codebuff (Opus 4.6)\n")
	fmt.Printf("  vibeproxy claudevibe -m claude-sonnet-4-6\n")
	fmt.Printf("                                   Launch via Codebuff with Sonnet 4.6\n")
	fmt.Printf("  claudevibe --model claude-sonnet-4-6\n")
	fmt.Printf("                                   Launch via Codebuff with a specific model\n")
	fmt.Printf("  vibeproxy status                 Check proxy and auth status\n")
	fmt.Println()
}

// printBanner prints the startup banner with proxy info.
func printBanner(cfg *config.Config) {
	fmt.Println()
	fmt.Printf("%s%s🔌 VibeProxy Linux v%s%s\n", colorBold, colorBlue, version, colorReset)
	fmt.Printf("   Proxy:   %shttp://127.0.0.1:%d%s\n", colorGreen, cfg.ProxyPort, colorReset)
	fmt.Printf("   Backend: %shttp://127.0.0.1:%d%s\n", colorGreen, cfg.BackendPort, colorReset)
	if cfg.VercelGatewayEnabled {
		fmt.Printf("   Vercel:  %s● Enabled%s\n", colorGreen, colorReset)
	}
	fmt.Println()
	fmt.Printf("   %sPress Ctrl+C to stop%s\n", colorYellow, colorReset)
	fmt.Println()
}

// spinner provides an animated CLI progress indicator.
type spinner struct {
	frames  []string
	message string
	stopCh  chan struct{}
	doneCh  chan struct{}
}

func newSpinner(message string) *spinner {
	return &spinner{
		frames:  []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		message: message,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

func (s *spinner) Start() {
	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		fmt.Fprintf(os.Stderr, "\r%s%s %s%s", colorYellow, s.frames[0], s.message, colorReset)
		i++
		for {
			select {
			case <-s.stopCh:
				fmt.Fprintf(os.Stderr, "\r\033[K")
				return
			case <-ticker.C:
				fmt.Fprintf(os.Stderr, "\r%s%s %s%s", colorYellow, s.frames[i%len(s.frames)], s.message, colorReset)
				i++
			}
		}
	}()
}

func (s *spinner) Stop() {
	close(s.stopCh)
	<-s.doneCh
}

// isProxyRunning checks the PID file first, then falls back to recovering the process from the proxy port.
func isProxyRunning() (bool, int) {
	cfg := loadConfigOrDefault()
	state := inspectRuntimeState(cfg)
	if state.Proxy.PID > 0 {
		return true, state.Proxy.PID
	}
	return probePort(cfg.ProxyPort), 0
}

// writePidFile writes the current process PID to the PID file.
func writePidFile() {
	pidPath := config.PidFilePath()
	pid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "%s⚠ Failed to write PID file: %v%s\n", colorYellow, err, colorReset)
	}
}

// removePidFile removes the PID file on shutdown.
func removePidFile() {
	os.Remove(config.PidFilePath())
}
