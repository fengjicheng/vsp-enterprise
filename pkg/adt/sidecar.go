package adt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SidecarConfig holds configuration for the Java JCo proxy sidecar.
type SidecarConfig struct {
	JcoProxyJar   string // Path to jco-proxy JAR
	JcoLibsDir    string // Path to JCo libraries (sapjco3.jar + native lib)
	JavaPath      string // Java binary path (default: "java")
	Port          int    // Fixed port (0 = auto-assign)
	MaxConcurrent int    // Max concurrent RFC requests (default: 5)

	// SAP connection parameters forwarded to the Java sidecar
	AsHost   string // Application server host (direct connection)
	SysNr    string // System number (direct connection)
	MsHost   string // Message server host (load-balanced)
	MsServ   string // Message server service/port
	R3Name   string // System ID for load-balanced
	Group    string // Logon group for load-balanced
	Client   string // SAP client number
	Username string // SAP username
	Password string // SAP password
	Language string // SAP language

	// JcoProperties holds arbitrary JCo destination properties.
	// When populated (e.g., for SNC/SSO), these are passed to the sidecar
	// as --jco.<property> <value> command-line arguments instead of the
	// individual named connection parameters above.
	JcoProperties map[string]string
}

// Validate checks that required fields are present.
func (c *SidecarConfig) Validate() error {
	if c.JcoProxyJar == "" {
		return fmt.Errorf("JcoProxyJar is required")
	}
	if _, err := os.Stat(c.JcoProxyJar); err != nil {
		return fmt.Errorf("JcoProxyJar not found: %w", err)
	}
	if c.JcoLibsDir == "" {
		return fmt.Errorf("JcoLibsDir is required")
	}
	if info, err := os.Stat(c.JcoLibsDir); err != nil {
		return fmt.Errorf("JcoLibsDir not found: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("JcoLibsDir is not a directory: %s", c.JcoLibsDir)
	}
	return nil
}

// SidecarManager manages the lifecycle of the Java JCo proxy sidecar process.
type SidecarManager struct {
	config     *SidecarConfig
	process    *os.Process
	cmd        *exec.Cmd
	actualPort int
	mu         sync.Mutex

	// STDIO connection pipes (only used when Transport == "stdio")
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stdioMu sync.Mutex // serializes STDIO request/response pairs
}

// NewSidecarManager creates a new SidecarManager with the given configuration.
func NewSidecarManager(cfg *SidecarConfig) *SidecarManager {
	if cfg.JavaPath == "" {
		cfg.JavaPath = "java"
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 5
	}
	return &SidecarManager{
		config: cfg,
	}
}

// Start launches the Java sidecar process and waits for it to be ready.
// It first kills any orphaned sidecar processes from previous runs.
func (s *SidecarManager) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.process != nil {
		return fmt.Errorf("sidecar already running (pid %d)", s.process.Pid)
	}

	// Kill any orphaned sidecar processes from previous runs
	s.killOrphanedSidecars()

	if err := s.config.Validate(); err != nil {
		return fmt.Errorf("invalid sidecar config: %w", err)
	}

	// Build classpath and args
	classpath := s.buildClasspath()
	args := s.buildArgs(classpath)

	cmd := exec.CommandContext(ctx, s.config.JavaPath, args...)

	// Set native library path
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("LD_LIBRARY_PATH=%s", s.config.JcoLibsDir),
		fmt.Sprintf("DYLD_LIBRARY_PATH=%s", s.config.JcoLibsDir),
	)

	return s.start(ctx, cmd)
}

// start launches the sidecar in STDIO mode and waits for SIDECAR_READY.
func (s *SidecarManager) start(ctx context.Context, cmd *exec.Cmd) error {
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting sidecar: %w", err)
	}

	s.cmd = cmd
	s.process = cmd.Process
	s.stdin = stdinPipe
	s.stdout = bufio.NewReader(stdoutPipe)

	// Wait for SIDECAR_READY line
	err = s.waitForReady(ctx)
	if err != nil {
		s.process.Kill()
		s.stdin = nil
		s.stdout = nil
		s.process = nil
		s.cmd = nil
		if errMsg := extractSidecarError(stderrBuf.String()); errMsg != "" {
			return fmt.Errorf("waiting for sidecar ready: %s", errMsg)
		}
		return fmt.Errorf("waiting for sidecar ready: %w", err)
	}
	return nil
}

// waitForReady reads stdout until it finds "SIDECAR_READY".
func (s *SidecarManager) waitForReady(ctx context.Context) error {
	readyCh := make(chan struct{}, 1)
	errCh := make(chan error, 1)

	go func() {
		for {
			line, err := s.stdout.ReadString('\n')
			if err != nil {
				errCh <- fmt.Errorf("reading sidecar stdout: %w", err)
				return
			}
			if strings.TrimSpace(line) == "SIDECAR_READY" {
				readyCh <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-readyCh:
		return nil
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for sidecar ready: %w", ctx.Err())
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timeout waiting for sidecar to start (30s)")
	}
}

// Stop gracefully shuts down the sidecar process.
func (s *SidecarManager) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.process == nil {
		return nil
	}

	// Close STDIO pipes first (signals EOF to the sidecar)
	if s.stdin != nil {
		s.stdin.Close()
		s.stdin = nil
	}
	s.stdout = nil

	// Send SIGTERM
	if err := s.process.Signal(os.Interrupt); err != nil {
		// Process may have already exited
		s.process = nil
		s.cmd = nil
		return nil
	}

	// Wait up to 5 seconds for graceful shutdown
	done := make(chan error, 1)
	go func() {
		if s.cmd != nil {
			done <- s.cmd.Wait()
		} else {
			_, err := s.process.Wait()
			done <- err
		}
	}()

	select {
	case <-done:
		// Exited gracefully
	case <-time.After(5 * time.Second):
		// Force kill
		s.process.Kill()
	}

	s.process = nil
	s.cmd = nil
	return nil
}

// IsRunning checks if the sidecar process is alive.
func (s *SidecarManager) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.process == nil {
		return false
	}

	// On Unix, sending signal 0 checks if process exists without actually signaling it
	err := s.process.Signal(os.Signal(nil))
	return err == nil
}

// SendSTDIO sends a JSON message to the sidecar via stdin and reads the response from stdout.
// The caller must provide a unique id to correlate the response. This is thread-safe.
func (s *SidecarManager) SendSTDIO(msg map[string]interface{}) (map[string]interface{}, error) {
	s.stdioMu.Lock()
	defer s.stdioMu.Unlock()

	if s.stdin == nil || s.stdout == nil {
		return nil, fmt.Errorf("STDIO connection not initialized")
	}

	// Write request as a single JSON line
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling STDIO request: %w", err)
	}
	data = append(data, '\n')

	if _, err := s.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("writing to sidecar stdin: %w", err)
	}

	// Read response line
	line, err := s.stdout.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading from sidecar stdout: %w", err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("parsing sidecar STDIO response: %w", err)
	}

	// Check for error field
	if errMsg, ok := resp["error"]; ok && errMsg != nil {
		return nil, fmt.Errorf("sidecar error: %v", errMsg)
	}

	return resp, nil
}

// killOrphanedSidecars finds and kills any RfcProxyServer processes left over from previous runs.
// Only truly orphaned sidecars (reparented to init/launchd, PPID 1) are killed. Sidecars whose
// parent is still alive belong to this run or to another vsp process running in parallel
// (e.g. a second MCP client session) and must not be touched.
func (s *SidecarManager) killOrphanedSidecars() {
	// Use pgrep to find Java processes running RfcProxyServer
	out, err := exec.Command("pgrep", "-f", "RfcProxyServer").Output()
	if err != nil {
		return // No matches or pgrep not available
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		if parentPID(pid) != 1 {
			continue // parent still alive, not orphaned
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "[INFO] Killing orphaned sidecar process (pid %d)\n", pid)
		proc.Kill()
		// Brief wait for process to exit
		time.Sleep(200 * time.Millisecond)
	}
}

// parentPID returns the parent PID of pid, or -1 if it cannot be determined.
func parentPID(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return -1
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return ppid
}

// buildClasspath constructs the Java classpath from the JAR and JCo libraries.
func (s *SidecarManager) buildClasspath() string {
	parts := []string{s.config.JcoProxyJar}

	// Add all JARs from JCo libs directory
	entries, err := os.ReadDir(s.config.JcoLibsDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".jar") {
				parts = append(parts, filepath.Join(s.config.JcoLibsDir, entry.Name()))
			}
		}
	}

	return strings.Join(parts, string(os.PathListSeparator))
}

// buildArgs constructs the Java command-line arguments.
func (s *SidecarManager) buildArgs(classpath string) []string {
	args := []string{
		"-cp", classpath,
		fmt.Sprintf("-Djava.library.path=%s", s.config.JcoLibsDir),
		"--enable-native-access=ALL-UNNAMED",
		"com.sap.mcp.proxy.RfcProxyServer",
	}

	// STDIO connection flag
	args = append(args, "--stdio")

	// If JcoProperties is populated (e.g., SNC/SSO mode), pass all connection
	// parameters as --<property> <value> arguments (keys already have jco.client.* prefix).
	// The Java sidecar collects these and uses them directly as JCo destination properties.
	// Also pass --user, --client, --lang as named args so Java can inject them if needed.
	if len(s.config.JcoProperties) > 0 {
		for k, v := range s.config.JcoProperties {
			args = append(args, "--"+k, v)
		}
		if s.config.Username != "" {
			args = append(args, "--user", s.config.Username)
		}
		if s.config.Client != "" {
			args = append(args, "--client", s.config.Client)
		}
		if s.config.Language != "" {
			args = append(args, "--lang", s.config.Language)
		}
		return args
	}

	// Standard named connection parameters (backward-compatible)
	if s.config.AsHost != "" {
		args = append(args, "--ashost", s.config.AsHost)
	}
	if s.config.SysNr != "" {
		args = append(args, "--sysnr", s.config.SysNr)
	}
	if s.config.MsHost != "" {
		args = append(args, "--mshost", s.config.MsHost)
	}
	if s.config.MsServ != "" {
		args = append(args, "--msserv", s.config.MsServ)
	}
	if s.config.R3Name != "" {
		args = append(args, "--r3name", s.config.R3Name)
	}
	if s.config.Group != "" {
		args = append(args, "--group", s.config.Group)
	}
	if s.config.Client != "" {
		args = append(args, "--client", s.config.Client)
	}
	if s.config.Username != "" {
		args = append(args, "--user", s.config.Username)
	}
	if s.config.Password != "" {
		args = append(args, "--password", s.config.Password)
	}
	if s.config.Language != "" {
		args = append(args, "--language", s.config.Language)
	}

	return args
}

// waitForPort reads stdout until it finds "SIDECAR_PORT=<port>" and returns the port.
func (s *SidecarManager) waitForPort(ctx context.Context, stdout io.Reader) (int, error) {
	scanner := bufio.NewScanner(stdout)
	portCh := make(chan int, 1)
	errCh := make(chan error, 1)

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if port, ok := parsePortLine(line); ok {
				portCh <- port
				return
			}
		}
		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("reading sidecar stdout: %w", err)
		} else {
			errCh <- fmt.Errorf("sidecar exited without reporting port")
		}
	}()

	select {
	case port := <-portCh:
		return port, nil
	case err := <-errCh:
		return 0, err
	case <-ctx.Done():
		return 0, fmt.Errorf("timeout waiting for sidecar port: %w", ctx.Err())
	case <-time.After(30 * time.Second):
		return 0, fmt.Errorf("timeout waiting for sidecar to start (30s)")
	}
}

// parsePortLine parses a "SIDECAR_PORT=<port>" line and returns the port number.
func parsePortLine(line string) (int, bool) {
	const prefix = "SIDECAR_PORT="
	if !strings.HasPrefix(line, prefix) {
		return 0, false
	}
	portStr := strings.TrimPrefix(line, prefix)
	portStr = strings.TrimSpace(portStr)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// extractSidecarError parses stderr output for "SIDECAR_ERROR: ..." lines.
func extractSidecarError(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "SIDECAR_ERROR: ") {
			return strings.TrimPrefix(line, "SIDECAR_ERROR: ")
		}
	}
	return ""
}
