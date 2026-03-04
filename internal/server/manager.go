package server

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vibeproxy/vibeproxy-linux/internal/config"
	"github.com/vibeproxy/vibeproxy-linux/internal/notify"
)

// --------------------------------------------------------------------
// Ring buffer – simple slice-based circular buffer for log lines.
// --------------------------------------------------------------------

type ringBuffer struct {
	storage []string
	head    int
	tail    int
	count   int
	cap     int
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity < 1 {
		capacity = 1
	}
	return &ringBuffer{
		storage: make([]string, capacity),
		cap:     capacity,
	}
}

func (rb *ringBuffer) append(s string) {
	rb.storage[rb.tail] = s
	if rb.count == rb.cap {
		rb.head = (rb.head + 1) % rb.cap
	} else {
		rb.count++
	}
	rb.tail = (rb.tail + 1) % rb.cap
}

func (rb *ringBuffer) elements() []string {
	if rb.count == 0 {
		return nil
	}
	out := make([]string, 0, rb.count)
	for i := 0; i < rb.count; i++ {
		idx := (rb.head + i) % rb.cap
		out = append(out, rb.storage[idx])
	}
	return out
}

// --------------------------------------------------------------------
// AuthCommand – iota-based enum for provider auth actions.
// --------------------------------------------------------------------

type AuthCommand int

const (
	AuthClaude AuthCommand = iota
	AuthCodex
	AuthCopilot
	AuthGemini
	AuthQwen
	AuthAntigravity
)

// AuthRequest carries the command and any provider-specific data (e.g. Qwen email).
type AuthRequest struct {
	Command AuthCommand
	Email   string // used for Qwen login
}

// --------------------------------------------------------------------
// Timing constants (mirrors Swift Timing enum).
// --------------------------------------------------------------------

const (
	readinessCheckDelay        = 1 * time.Second
	gracefulTerminationTimeout = 2 * time.Second
	terminationPollInterval    = 50 * time.Millisecond
	maxLogLines                = 1000
)

// --------------------------------------------------------------------
// Manager – manages the cli-proxy-api-plus subprocess on Linux.
// --------------------------------------------------------------------

type Manager struct {
	BinaryPath string
	ConfigPath string
	Port       int // backend port, default 8318

	OnStatusChange func(bool)
	OnLogUpdate    func([]string)

	cmd       *exec.Cmd
	mu        sync.Mutex
	running   bool
	logger    *log.Logger
	logBuffer *ringBuffer
}

// NewManager creates a new Manager. Port defaults to 8318 when 0.
func NewManager(binaryPath, configPath string, port int) *Manager {
	if port == 0 {
		port = 8318
	}
	return &Manager{
		BinaryPath: binaryPath,
		ConfigPath: configPath,
		Port:       port,
		logger:     log.New(os.Stderr, "[ServerManager] ", log.LstdFlags),
		logBuffer:  newRingBuffer(maxLogLines),
	}
}

// --------------------------------------------------------------------
// Start / Stop / IsRunning
// --------------------------------------------------------------------

// Start launches the backend subprocess.
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Clean up orphaned processes from previous crashes.
	m.KillOrphanedProcesses()

	// Validate binary exists.
	if _, err := os.Stat(m.BinaryPath); os.IsNotExist(err) {
		m.addLog(fmt.Sprintf("❌ Error: cli-proxy-api-plus binary not found at %s", m.BinaryPath))
		return fmt.Errorf("binary not found: %s", m.BinaryPath)
	}

	// Validate config exists.
	if m.ConfigPath == "" {
		m.addLog("❌ Error: config path is empty")
		return fmt.Errorf("config path is empty")
	}
	if _, err := os.Stat(m.ConfigPath); os.IsNotExist(err) {
		m.addLog("❌ Error: config.yaml not found")
		return fmt.Errorf("config not found: %s", m.ConfigPath)
	}

	cmd := exec.Command(m.BinaryPath, "-config", m.ConfigPath)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		m.addLog(fmt.Sprintf("❌ Failed to start server: %v", err))
		return fmt.Errorf("starting process: %w", err)
	}

	m.mu.Lock()
	m.cmd = cmd
	m.running = true
	m.mu.Unlock()

	m.addLog(fmt.Sprintf("✓ Server started on port %d", m.Port))

	// Write backend PID file (separate from the main vibeproxy PID file).
	if pidPath := config.BackendPidFilePath(); pidPath != "" {
		_ = os.MkdirAll(filepath.Dir(pidPath), 0755)
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)
	}

	if m.OnStatusChange != nil {
		m.OnStatusChange(true)
	}

	// Goroutines to capture stdout / stderr.
	go m.scanPipe(stdoutPipe, "")
	go m.scanPipe(stderrPipe, "⚠️ ")

	// Background goroutine to monitor process exit.
	go func() {
		_ = cmd.Wait()

		m.mu.Lock()
		m.running = false
		m.cmd = nil
		m.mu.Unlock()

		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		m.addLog(fmt.Sprintf("Server stopped with code: %d", exitCode))

		// Clean up backend PID file.
		if pidPath := config.BackendPidFilePath(); pidPath != "" {
			_ = os.Remove(pidPath)
		}

		if m.OnStatusChange != nil {
			m.OnStatusChange(false)
		}
	}()

	// Readiness check: wait 1 s then verify the process is still alive.
	time.Sleep(readinessCheckDelay)

	m.mu.Lock()
	alive := m.running
	m.mu.Unlock()

	if !alive {
		m.addLog("⚠️ Server exited before becoming ready")
		return fmt.Errorf("server exited before becoming ready")
	}

	return nil
}

// Stop sends SIGTERM, polls up to 2 s, then SIGKILL if necessary.
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		m.mu.Lock()
		m.running = false
		m.cmd = nil
		m.mu.Unlock()
		if m.OnStatusChange != nil {
			m.OnStatusChange(false)
		}
		return nil
	}

	pid := cmd.Process.Pid
	m.addLog(fmt.Sprintf("Stopping server (PID: %d)...", pid))

	// Graceful SIGTERM.
	_ = cmd.Process.Signal(syscall.SIGTERM)

	// Poll for up to gracefulTerminationTimeout.
	deadline := time.Now().Add(gracefulTerminationTimeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		still := m.running
		m.mu.Unlock()
		if !still {
			m.addLog("✓ Server stopped")
			return nil
		}
		time.Sleep(terminationPollInterval)
	}

	// Force kill if still running.
	m.mu.Lock()
	still := m.running
	m.mu.Unlock()
	if still {
		m.addLog("⚠️ Server didn't stop gracefully, force killing...")
		_ = cmd.Process.Signal(syscall.SIGKILL)

		// Wait for the background goroutine to detect exit (up to 1s).
		killDeadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(killDeadline) {
			m.mu.Lock()
			exited := !m.running
			m.mu.Unlock()
			if exited {
				break
			}
			time.Sleep(terminationPollInterval)
		}

		m.mu.Lock()
		m.running = false
		m.cmd = nil
		m.mu.Unlock()
	}

	// Clean up backend PID file.
	if pidPath := config.BackendPidFilePath(); pidPath != "" {
		_ = os.Remove(pidPath)
	}

	m.addLog("✓ Server stopped")
	if m.OnStatusChange != nil {
		m.OnStatusChange(false)
	}
	return nil
}

// IsRunning returns the current status in a thread-safe manner.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// --------------------------------------------------------------------
// Orphan cleanup
// --------------------------------------------------------------------

// KillOrphanedProcesses reads the backend PID file and uses pgrep/pkill to
// find and kill any leftover cli-proxy-api-plus processes.
func (m *Manager) KillOrphanedProcesses() {
	// Try backend PID file first.
	if pidPath := config.BackendPidFilePath(); pidPath != "" {
		if data, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if proc, err := os.FindProcess(pid); err == nil {
					// Signal 0 checks existence.
					if proc.Signal(syscall.Signal(0)) == nil {
						m.addLog(fmt.Sprintf("⚠️ Found orphaned server process from PID file: %d", pid))
						_ = proc.Signal(syscall.SIGKILL)
						time.Sleep(500 * time.Millisecond)
					}
				}
			}
			_ = os.Remove(pidPath)
		}
	}

	// Also use pgrep to find any remaining orphans.
	out, err := exec.Command("pgrep", "-f", "cli-proxy-api-plus").Output()
	if err != nil {
		// Exit code 1 means no processes found – that's fine.
		return
	}
	pids := strings.TrimSpace(string(out))
	if pids == "" {
		return
	}

	m.addLog(fmt.Sprintf("⚠️ Found orphaned server process(es): %s",
		strings.ReplaceAll(pids, "\n", ", ")))

	_ = exec.Command("pkill", "-9", "-f", "cli-proxy-api-plus").Run()
	time.Sleep(500 * time.Millisecond)
	m.addLog("✓ Cleaned up orphaned processes")
}

// --------------------------------------------------------------------
// Auth commands
// --------------------------------------------------------------------

// RunAuthCommand executes the binary with provider-specific auth flags.
// It returns a user-facing message and any error.
func (m *Manager) RunAuthCommand(req AuthRequest) (string, error) {
	if _, err := os.Stat(m.BinaryPath); os.IsNotExist(err) {
		return "", fmt.Errorf("binary not found: %s", m.BinaryPath)
	}

	args := []string{"--config", m.ConfigPath}

	switch req.Command {
	case AuthClaude:
		args = append(args, "-claude-login")
	case AuthCodex:
		args = append(args, "-codex-login")
	case AuthCopilot:
		args = append(args, "-github-copilot-login")
	case AuthGemini:
		args = append(args, "-login")
	case AuthQwen:
		args = append(args, "-qwen-login")
	case AuthAntigravity:
		args = append(args, "-antigravity-login")
	}

	cmd := exec.Command(m.BinaryPath, args...)
	cmd.Env = os.Environ()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("creating stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("creating stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("creating stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting auth process: %w", err)
	}

	m.addLog(fmt.Sprintf("✓ Authentication process started (PID: %d) - browser should open shortly", cmd.Process.Pid))

	// Channel closed when the process exits.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	// Captured output (for Copilot device code extraction).
	var captureMu sync.Mutex
	var capturedOutput strings.Builder

	// Goroutine to read stdout.
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			captureMu.Lock()
			capturedOutput.WriteString(line + "\n")
			captureMu.Unlock()
			m.logger.Printf("[Auth] output: %s", line)
		}
	}()

	// Goroutine to read stderr.
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			captureMu.Lock()
			capturedOutput.WriteString(line + "\n")
			captureMu.Unlock()
			m.logger.Printf("[Auth] stderr: %s", line)
		}
	}()

	// Provider-specific stdin interactions.
	switch req.Command {
	case AuthGemini:
		// Send newline after 3 s to accept default project.
		go func() {
			select {
			case <-done:
				return
			case <-time.After(3 * time.Second):
				_, _ = io.WriteString(stdinPipe, "\n")
				m.logger.Println("[Auth] Sent newline to accept default project")
			}
		}()

	case AuthCodex:
		// Send newline after 12 s to keep waiting for callback.
		go func() {
			select {
			case <-done:
				return
			case <-time.After(12 * time.Second):
				_, _ = io.WriteString(stdinPipe, "\n")
				m.logger.Println("[Auth] Sent newline to keep Codex login waiting for callback")
			}
		}()

	case AuthQwen:
		// Send email after 10 s once OAuth browser flow completes.
		if req.Email != "" {
			go func() {
				select {
				case <-done:
					return
				case <-time.After(10 * time.Second):
					_, _ = io.WriteString(stdinPipe, req.Email+"\n")
					m.logger.Printf("[Auth] Sent Qwen email: %s", req.Email)
				}
			}()
		}

	case AuthCopilot:
		// Handled below after waiting for output.
	}

	// Quick crash detection: wait briefly to see if the process exits immediately.
	quickWait := 1 * time.Second
	if req.Command == AuthCopilot {
		quickWait = 2 * time.Second
	}

	const authTimeout = 5 * time.Minute

	select {
	case <-done:
		// Process exited quickly – check exit code and output.
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
			return "✓ Authentication completed.", nil
		}

		captureMu.Lock()
		output := capturedOutput.String()
		captureMu.Unlock()

		if strings.Contains(output, "Opening browser") || strings.Contains(output, "Attempting to open URL") {
			// Browser opened but process finished quickly – possible success.
			return "🌐 Browser opened for authentication.\n\nPlease complete the login in your browser.", nil
		}

		msg := strings.TrimSpace(output)
		if msg == "" {
			msg = "Authentication process failed unexpectedly"
		}
		return "", fmt.Errorf("%s", msg)

	case <-time.After(quickWait):
		// Process still running – browser likely opened, waiting for OAuth callback.
		m.logger.Println("[Auth] Browser opened, waiting for authentication to complete...")

		// For Copilot, extract and display the device code.
		var earlyMsg string
		if req.Command == AuthCopilot {
			captureMu.Lock()
			output := capturedOutput.String()
			captureMu.Unlock()

			code := extractDeviceCode(output)
			if code != "" {
				_ = notify.CopyToClipboard(code)
				earlyMsg = fmt.Sprintf("📋 Code copied to clipboard: %s\nPaste it in the browser!\n", code)
				m.addLog(fmt.Sprintf("📋 Copilot device code: %s (copied to clipboard)", code))
			}
		}

		// Block until the auth process finishes or times out.
		select {
		case <-done:
			if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
				return earlyMsg + "✓ Authentication completed successfully.", nil
			}
			captureMu.Lock()
			output := capturedOutput.String()
			captureMu.Unlock()
			msg := strings.TrimSpace(output)
			if msg == "" {
				exitCode := -1
				if cmd.ProcessState != nil {
					exitCode = cmd.ProcessState.ExitCode()
				}
				msg = fmt.Sprintf("authentication process exited with code %d", exitCode)
			}
			return "", fmt.Errorf("%s", msg)

		case <-time.After(authTimeout):
			_ = cmd.Process.Kill()
			return "", fmt.Errorf("authentication timed out after 5 minutes")
		}
	}
}

// extractDeviceCode looks for "enter the code: XXXX-XXXX" in the output.
func extractDeviceCode(output string) string {
	const marker = "enter the code:"
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		idx := strings.Index(strings.ToLower(line), strings.ToLower(marker))
		if idx >= 0 {
			code := strings.TrimSpace(line[idx+len(marker):])
			if code != "" {
				return code
			}
		}
	}
	return ""
}

// --------------------------------------------------------------------
// Logging helpers
// --------------------------------------------------------------------

// GetLogs returns the current log buffer contents.
func (m *Manager) GetLogs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logBuffer.elements()
}

// addLog appends a timestamped line to the ring buffer and fires the callback.
func (m *Manager) addLog(message string) {
	ts := time.Now().Format("15:04:05")
	line := fmt.Sprintf("[%s] %s", ts, message)

	m.mu.Lock()
	m.logBuffer.append(line)
	logs := m.logBuffer.elements()
	m.mu.Unlock()

	m.logger.Println(message)

	if m.OnLogUpdate != nil {
		m.OnLogUpdate(logs)
	}
}

// scanPipe reads lines from a pipe and feeds them into addLog.
func (m *Manager) scanPipe(r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	// Allow longer lines (default 64 KiB token limit can be hit by verbose output).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		m.addLog(prefix + scanner.Text())
	}
}
