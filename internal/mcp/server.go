// Package mcp provides the MCP server implementation for ABAP ADT tools.
// server.go contains the Server singleton responsible for bootstrapping the MCP server,
// managing system connections via the Router, and handling setup/shutdown lifecycle.
package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/oisee/vibing-steampunk/embedded/deps"
	"github.com/oisee/vibing-steampunk/internal/config"
	"github.com/oisee/vibing-steampunk/pkg/adt"
)

// Server is the singleton MCP server instance.
// It bootstraps the mcp-go server, manages individual System connections
// via the Router, and handles setup/shutdown lifecycle.
type Server struct {
	mcpServer *server.MCPServer
	router    *Router
	config    *config.GlobalConfig // top-level configuration
}

// newMCPServer creates the underlying mcp-go MCPServer instance.
func newMCPServer() *server.MCPServer {
	return server.NewMCPServer(
		"mcp-abap-adt-go",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
		server.WithLogging(),
	)
}

// NewServer creates a new MCP server from a fully populated GlobalConfig.
// All systems (single or multi) must already be present in globalCfg.Systems.
// runtimeCookies is an optional per-system cookie map (e.g. from CLI browser auth).
// NewServer is a pure instantiation step with no network I/O; call Connect then Start before ServeStdio.
func NewServer(globalCfg *config.GlobalConfig, runtimeCookies map[string]map[string]string) (*Server, error) {
	if len(globalCfg.Systems) == 0 {
		return nil, fmt.Errorf("no systems configured")
	}

	// Store the global config as a singleton so that Build* methods and
	// other code can access global settings without field copying.
	config.SetInstance(globalCfg)

	mcpSrv := newMCPServer()
	router := NewRouter(mcpSrv)

	proxyJarChecked := false

	for sysID, sysCfg := range globalCfg.Systems {
		// Ensure proxy JAR is available for RFC mode (one-time check)
		if strings.EqualFold(sysCfg.ConnectionMode, "rfc") && !proxyJarChecked {
			proxyJarChecked = true
			if err := ensureProxyJARIsAvailable(globalCfg); err != nil {
				return nil, fmt.Errorf("failed to ensure proxy JAR availability for system %q: %w", sysID, err)
			}
		}

		// Resolve runtime cookies (file, string, or browser auth)
		cookies, err := resolveSystemCookies(sysID, &sysCfg, globalCfg.Verbose, runtimeCookies)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve cookies for system %q: %w", sysID, err)
		}

		// Instantiate the system (pure allocation, no connection or activation)
		sys, err := newSystemInstance(sysCfg, cookies)
		if err != nil {
			return nil, fmt.Errorf("failed to create system for %q: %w", sysID, err)
		}
		sys.id = sysID

		router.AddSystem(sysID, sys)

		if globalCfg.Verbose {
			connInfo := sysCfg.URL
			if strings.EqualFold(sysCfg.ConnectionMode, "rfc") {
				connInfo = fmt.Sprintf("RFC(%s)", sysCfg.AsHost)
				if sysCfg.MsHost != "" {
					connInfo = fmt.Sprintf("RFC-LB(%s)", sysCfg.MsHost)
				}
			}
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Instantiated system %q → %s (user: %s)\n",
				sysID, connInfo, sysCfg.Username)
		}
	}

	// NOTE: RegisterTools is NOT called here — it must be called after Connect()
	// so that ADT discovery results are available for endpoint-based tool filtering.

	return &Server{
		mcpServer: mcpSrv,
		router:    router,
		config:    globalCfg,
	}, nil
}

// RegisterTools registers all tools with permission-based filtering.
// It is called by ConnectAsync before background discovery runs, so no
// endpoint-based filtering is applied at registration time.
func (s *Server) RegisterTools() {
	s.router.RegisterTools(s.config, s.router.systems)
}

// ConnectAsync prepares the server to serve immediately without blocking on slow
// SAP/JCo startup. It registers the full (permission-filtered) tool set right away
// so the MCP handshake and tools/list respond instantly, then warms up each
// system's connection (JCo sidecar startup + endpoint discovery + runtime
// activation) in the background. Tool handlers block on System.EnsureReady until
// the target system finishes initializing, so the first call may be slow but
// succeeds instead of racing an unready server.
//
// Because discovery has not run yet when tools are registered, no endpoint-based
// filtering is applied at registration time; the full permission-enabled set is
// exposed. Endpoint availability is still enforced per call once the system is
// ready via the runtime permission checks in the router.
func (s *Server) ConnectAsync() {
	if s.router == nil {
		return
	}

	s.RegisterTools()

	for _, sys := range s.router.systems {
		sys.InitAsync()
	}
}

// resolveSystemCookies resolves runtime cookies for a system in this precedence:
// 1) runtimeCookies (injected, e.g. browser-auth from CLI)
// 2) cookie_file from config
// 3) cookie_string from config
// 4) browser_auth from config
func resolveSystemCookies(systemID string, sysCfg *config.SystemConfig, verbose bool, runtimeCookies map[string]map[string]string) (map[string]string, error) {
	if sysCfg.BrowserAuth && sysCfg.BrowserExec == "" && strings.TrimSpace(sysCfg.BrowserAuthURL) == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		apiURL, err := adt.ResolveBrowserAuthAPIURL(ctx, sysCfg.URL, sysCfg.Insecure)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve ADT API URL: %w", err)
		}
		sysCfg.URL = apiURL
	}

	if runtimeCookies != nil {
		if c := runtimeCookies[systemID]; len(c) > 0 {
			return c, nil
		}
	}

	if sysCfg.CookieFile != "" {
		cookies, err := adt.LoadCookiesFromFile(sysCfg.CookieFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load cookies from file %q: %w", sysCfg.CookieFile, err)
		}
		if len(cookies) > 0 {
			return cookies, nil
		}
	}

	if sysCfg.CookieString != "" {
		cookies := adt.ParseCookieString(sysCfg.CookieString)
		if len(cookies) > 0 {
			return cookies, nil
		}
		return nil, fmt.Errorf("failed to parse cookie_string for system %q", systemID)
	}

	if sysCfg.BrowserAuth {
		if sysCfg.URL == "" {
			return nil, fmt.Errorf("browser_auth requires url")
		}

		timeout := 120 * time.Second
		if sysCfg.BrowserAuthTimeout != "" {
			if d, err := time.ParseDuration(sysCfg.BrowserAuthTimeout); err == nil {
				timeout = d
			}
		}

		if verbose || sysCfg.Verbose {
			_, _ = fmt.Fprintf(os.Stderr, "[BROWSER-AUTH] Starting browser login for system %q (%s)\n", systemID, sysCfg.URL)
		}

		cookies, err := adt.BrowserLoginWithTargetForClient(context.Background(), sysCfg.URL, sysCfg.BrowserAuthURL, sysCfg.Client, sysCfg.Language, sysCfg.Insecure, timeout, sysCfg.BrowserExec, verbose || sysCfg.Verbose)
		if err != nil {
			return nil, fmt.Errorf("browser authentication failed: %w", err)
		}

		if sysCfg.CookieSave != "" {
			if err := adt.SaveCookiesToFile(cookies, sysCfg.URL, sysCfg.CookieSave); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "[BROWSER-AUTH] Warning: failed to save cookies for system %q: %v\n", systemID, err)
			} else {
				_, _ = fmt.Fprintf(os.Stderr, "[BROWSER-AUTH] Cookies for system %q saved to %s\n", systemID, sysCfg.CookieSave)
			}
		}

		return cookies, nil
	}

	return nil, nil
}

func ensureProxyJARIsAvailable(cfg *config.GlobalConfig) error {
	if cfg.JcoProxyJar != "" {
		if !fileExists(cfg.JcoProxyJar) {
			return fmt.Errorf("proxy JAR file %s supplied by user does not exist", cfg.JcoProxyJar)
		}

		return nil
	}

	data := deps.GetEmbeddedProxyJar()
	if data == nil {
		panic("Embedded proxy JAR not found")
	}

	extractDir := cfg.JcoLibsDir
	if extractDir == "" {
		extractDir = "./jco-libs"
	}
	proxyPath := filepath.Join(extractDir, "jco-proxy.jar")

	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return fmt.Errorf("failed to create target directory for proxy JAR: %s, %v", proxyPath, err)
	}

	// Another vsp process may have this JAR open in its JCo sidecar JVM. Truncating it
	// in place would corrupt that JVM's lazy class loading, so skip identical content
	// and otherwise replace the file atomically.
	if existing, err := os.ReadFile(proxyPath); err == nil && bytes.Equal(existing, data) {
		cfg.JcoProxyJar = proxyPath
		return nil
	}
	if err := writeFileAtomic(proxyPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write proxy JAR to %s: %v", proxyPath, err)
	}

	cfg.JcoProxyJar = proxyPath

	if cfg.Verbose {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Auto-extracted embedded proxy JAR to %s\n", proxyPath)
	}

	return nil
}

// writeFileAtomic writes data to a temp file in the target directory and renames it
// over path, so readers holding the old file open keep seeing consistent content.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// fileExists returns true if the path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Shutdown gracefully stops the server and cleans up all system resources.
// Idempotent and safe to call multiple times. Collects errors from all systems
// and returns the first non-nil error, but continues cleanup for remaining systems.
func (s *Server) Shutdown() error {
	if s.router == nil {
		return nil
	}

	var firstErr error
	for sysID, sys := range s.router.systems {
		if err := sys.Shutdown(); err != nil {
			if s.config.Verbose {
				_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Warning: shutdown error for system %q: %v\n", sysID, err)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ServeStdio starts the MCP server on stdin/stdout.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.mcpServer)
}
