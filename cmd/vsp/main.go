// vsp is an MCP server providing ABAP Development Tools (ADT) functionality.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/oisee/vibing-steampunk/internal"
	"github.com/oisee/vibing-steampunk/internal/config"
	"github.com/oisee/vibing-steampunk/internal/daemon"
	"github.com/oisee/vibing-steampunk/internal/mcp"
	"github.com/oisee/vibing-steampunk/pkg/adt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	// Version information (set by build flags)
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

var cfg = &config.GlobalConfig{}

// singleSys accumulates per-system settings from CLI flags / env vars.
// In single-system mode it is stored as cfg.Systems["default"] before
// the server is created. In multi-system mode it is ignored.
var singleSys = &config.SystemConfig{}

// Multi-system mode (CLI-only flag for argument validation)
var (
	multiSystem bool
	configFile  string
)

// runtimeCookies holds per-system cookies acquired at runtime (e.g. browser auth).
// They are intentionally not part of persistent configuration.
var runtimeCookies = map[string]map[string]string{}

var rootCmd = &cobra.Command{
	Use:   "vsp",
	Short: "ABAP Development Tools for AI agents and DevOps",
	Long: `vsp — ABAP Development Tools for AI agents and DevOps.

Single binary, 9 platforms, no dependencies. Download from GitHub releases,
point your MCP config at it, done.

Two ways to use vsp:

  MCP Server (default)  Connects Claude, Gemini CLI, Copilot, Codex, Qwen Code,
						and other MCP-compatible agents to SAP systems.
						81 tools (focused), 122 (expert), or 1 universal tool (hyperfocused).

  CLI Utilities         Manage named system profiles, config files, and JCo setup
						from the terminal. Use --system / --multi-system to pick
						saved connections when starting the MCP server.

Quick start:
  # 1. MCP server (reads .env or SAP_* env vars)
  vsp --url https://host:44300 --user dev --password secret

  # 2. Start the server with a saved system profile
  vsp --system dev --verbose

  # 3. Terminal utilities
  vsp systems
  vsp config show

  # 4. Enterprise safety (hand to AI without fear)
  vsp --read-only                                    # no writes at all
  vsp --allowed-packages 'Z*,$TMP' --block-free-sql  # sandbox AI to custom code
  vsp --disallowed-ops CDUA                           # block create/delete/update/activate

Configuration files:
  .env          Default SAP connection (MCP server mode). SAP_URL, SAP_USER, etc.
  .vsp.json     Named system profiles for --system and --multi-system operation.
  .mcp.json     MCP server entries for Claude Desktop / other MCP clients.

  vsp config init       Generate example files (.env.example, .vsp.json.example, .mcp.json.example)
  vsp config show       Display effective configuration
  vsp config mcp-to-vsp Import systems from .mcp.json into .vsp.json
  vsp config vsp-to-mcp Export .vsp.json systems to .mcp.json format
  vsp config tools      Manage per-tool visibility in .vsp.json

Configuration priority: CLI flags > env vars > .env file > defaults
Ready-to-use configs for 8 AI agents: docs/cli-agents/`,
	Version:      fmt.Sprintf("%s (commit: %s, built: %s)", Version, Commit, BuildDate),
	SilenceUsage: true,
	RunE:         runServer,
}

func init() {
	// Load .env file if it exists
	_ = godotenv.Load()

	// Service URL
	rootCmd.Flags().StringVar(&singleSys.URL, "url", "", "SAP system URL (e.g., https://host:44300)")
	rootCmd.Flags().StringVar(&singleSys.URL, "service", "", "SAP system URL (alias for --url)")

	// Authentication flags
	rootCmd.Flags().StringVarP(&singleSys.Username, "user", "u", "", "SAP username")
	rootCmd.Flags().StringVarP(&singleSys.Password, "password", "p", "", "SAP password")
	rootCmd.Flags().StringVar(&singleSys.Password, "pass", "", "SAP password (alias for --password)")

	// SAP connection options
	rootCmd.Flags().StringVar(&singleSys.Client, "client", "001", "SAP client number")
	rootCmd.Flags().StringVar(&singleSys.Language, "language", "EN", "SAP language")
	rootCmd.Flags().BoolVar(&singleSys.Insecure, "insecure", false, "Skip TLS certificate verification")

	// Cookie authentication
	rootCmd.Flags().StringVar(&singleSys.CookieFile, "cookie-file", "", "Path to cookie file in Netscape format")
	rootCmd.Flags().StringVar(&singleSys.CookieString, "cookie-string", "", "Cookie string (key1=val1; key2=val2)")

	// Browser-based SSO authentication
	rootCmd.Flags().Bool("browser-auth", false, "Open browser for SSO login (Kerberos, SAML, Keycloak)")
	rootCmd.Flags().Duration("browser-auth-timeout", 120*time.Second, "Timeout for browser-based SSO login")
	rootCmd.Flags().String("browser-exec", "", "Use automated browser auth with this Chromium executable instead of the system browser callback")
	rootCmd.Flags().String("browser-auth-url", "", "Override browser login URL (absolute URL or path appended to --url); default: /sap/bc/adt/")
	rootCmd.Flags().String("cookie-save", "", "Save browser auth cookies to file for reuse with --cookie-file")

	// Session keep-alive
	rootCmd.Flags().Duration("keepalive", 5*time.Minute, "Session keep-alive interval (e.g., 60s, 5m). Prevents session timeout during idle periods. 0 = disabled")

	// Safety options
	rootCmd.Flags().BoolVar(&cfg.ReadOnly, "read-only", false, "Block all write operations (create, update, delete, activate)")
	rootCmd.Flags().BoolVar(&cfg.BlockFreeSQL, "block-free-sql", false, "Block execution of arbitrary SQL queries via RunQuery")
	rootCmd.Flags().StringVar(&cfg.AllowedOps, "allowed-ops", "", "Whitelist of allowed operation types (e.g., \"RSQ\" for Read, Search, Query only)")
	rootCmd.Flags().StringVar(&cfg.DisallowedOps, "disallowed-ops", "", "Blacklist of operation types to block (e.g., \"CDUA\" for Create, Delete, Update, Activate)")
	rootCmd.Flags().StringSliceVar(&cfg.AllowedPackages, "allowed-packages", nil, "Restrict operations to specific packages (comma-separated, supports wildcards like Z*)")
	rootCmd.Flags().BoolVar(&cfg.EnableTransports, "enable-transports", false, "Enable connection management operations (disabled by default for safety)")
	rootCmd.Flags().BoolVar(&cfg.TransportReadOnly, "connection-read-only", false, "Only allow read operations on transports (list, get)")
	rootCmd.Flags().StringSliceVar(&cfg.AllowedTransports, "allowed-transports", nil, "Restrict connection operations to specific transports (comma-separated, supports wildcards like A4HK*)")
	rootCmd.Flags().BoolVar(&cfg.AllowTransportableEdits, "allow-transportable-edits", false, "Allow editing objects in transportable packages (requires connection parameter)")

	// Mode options
	rootCmd.Flags().StringVar(&cfg.Mode, "mode", "focused", "Tool mode: focused (81 tools), expert (122 tools), or hyperfocused (single universal SAP tool)")
	rootCmd.Flags().StringVar(&cfg.DisabledGroups, "disabled-groups", "", "Disable tool groups: 5/U=UI5, T=Tests, H=HANA, D=Debug (e.g., \"TH\" disables Tests and HANA)")

	// Multi-system mode
	rootCmd.Flags().BoolVar(&multiSystem, "multi-system", false, "Enable multi-system mode: route tool requests to systems from .vsp.json config")
	rootCmd.Flags().StringVar(&configFile, "config", "", "Path to .vsp.json configuration file (auto-discovered if not set)")

	// Feature configuration (safety network)
	// Values: "auto" (default), "on", "off"
	rootCmd.Flags().StringVar(&cfg.FeatureHANA, "feature-hana", "auto", "HANA database detection: auto, on, off")
	rootCmd.Flags().StringVar(&cfg.FeatureAbapGit, "feature-abapgit", "auto", "abapGit integration: auto, on, off")
	rootCmd.Flags().StringVar(&cfg.FeatureRAP, "feature-rap", "auto", "RAP/OData development: auto, on, off")
	rootCmd.Flags().StringVar(&cfg.FeatureAMDP, "feature-amdp", "auto", "AMDP/HANA debugger: auto, on, off")
	rootCmd.Flags().StringVar(&cfg.FeatureUI5, "feature-ui5", "auto", "UI5/Fiori BSP management: auto, on, off")
	rootCmd.Flags().StringVar(&cfg.FeatureTransport, "feature-connection", "auto", "CTS connection management: auto, on, off")

	// Debugger configuration
	rootCmd.Flags().StringVar(&cfg.TerminalID, "terminal-id", "", "SAP GUI terminal ID for cross-tool breakpoint sharing")

	// RFC connection settings
	rootCmd.Flags().StringVar(&singleSys.ConnectionMode, "connection-mode", "http", "Connection mode: http (default) or rfc")
	rootCmd.Flags().StringVar(&singleSys.AsHost, "ashost", "", "SAP application server hostname (RFC mode)")
	rootCmd.Flags().StringVar(&singleSys.SysNr, "sysnr", "00", "SAP system number (RFC mode)")
	rootCmd.Flags().StringVar(&singleSys.MsHost, "mshost", "", "SAP message server host (RFC load balancing)")
	rootCmd.Flags().StringVar(&singleSys.MsServ, "msserv", "", "SAP message server service/port (RFC load balancing)")
	rootCmd.Flags().StringVar(&singleSys.R3Name, "r3name", "", "SAP system name (RFC load balancing)")
	rootCmd.Flags().StringVar(&singleSys.Group, "group", "", "SAP logon group (RFC load balancing)")
	rootCmd.Flags().StringVar(&cfg.JcoProxyJar, "jco-proxy-jar", "", "Path to jco-proxy JAR file")
	rootCmd.Flags().StringVar(&cfg.JcoLibsDir, "jco-libs-dir", "", "Path to JCo libraries directory")
	rootCmd.Flags().StringVar(&cfg.JavaPath, "java-path", "java", "Path to Java binary")
	rootCmd.Flags().IntVar(&cfg.RfcProxyPort, "rfc-proxy-port", 0, "Fixed sidecar port (0=auto)")
	rootCmd.Flags().IntVar(&cfg.RfcMaxConcurrent, "rfc-max-concurrent", 5, "Max concurrent RFC calls")

	// SNC/SSO configuration (via SAP UI Landscape)
	rootCmd.Flags().BoolVar(&singleSys.SNC, "snc", false, "Enable SNC single sign-on via JCo (requires --sysid)")
	rootCmd.Flags().StringVar(&singleSys.SysID, "sysid", "", "SAP System ID for SNC logon (3-char SID, reads connection from SAP UI Landscape)")
	rootCmd.Flags().StringVar(&cfg.LandscapeFile, "landscape-file", "", "Path to SAP UI Landscape XML (auto-discovered if not set)")

	// Output options
	rootCmd.Flags().BoolVarP(&cfg.Verbose, "verbose", "v", false, "Enable verbose output to stderr")

	// Bind flags to viper for environment variable support
	_ = viper.BindPFlag("url", rootCmd.Flags().Lookup("url"))
	_ = viper.BindPFlag("user", rootCmd.Flags().Lookup("user"))
	_ = viper.BindPFlag("password", rootCmd.Flags().Lookup("password"))
	_ = viper.BindPFlag("client", rootCmd.Flags().Lookup("client"))
	_ = viper.BindPFlag("language", rootCmd.Flags().Lookup("language"))
	_ = viper.BindPFlag("insecure", rootCmd.Flags().Lookup("insecure"))
	_ = viper.BindPFlag("cookie-file", rootCmd.Flags().Lookup("cookie-file"))
	_ = viper.BindPFlag("cookie-string", rootCmd.Flags().Lookup("cookie-string"))
	_ = viper.BindPFlag("browser-auth", rootCmd.Flags().Lookup("browser-auth"))
	_ = viper.BindPFlag("browser-auth-timeout", rootCmd.Flags().Lookup("browser-auth-timeout"))
	_ = viper.BindPFlag("browser-exec", rootCmd.Flags().Lookup("browser-exec"))
	_ = viper.BindPFlag("browser-auth-url", rootCmd.Flags().Lookup("browser-auth-url"))
	_ = viper.BindPFlag("cookie-save", rootCmd.Flags().Lookup("cookie-save"))
	_ = viper.BindPFlag("keepalive", rootCmd.Flags().Lookup("keepalive"))
	_ = viper.BindPFlag("read-only", rootCmd.Flags().Lookup("read-only"))
	_ = viper.BindPFlag("block-free-sql", rootCmd.Flags().Lookup("block-free-sql"))
	_ = viper.BindPFlag("allowed-ops", rootCmd.Flags().Lookup("allowed-ops"))
	_ = viper.BindPFlag("disallowed-ops", rootCmd.Flags().Lookup("disallowed-ops"))
	_ = viper.BindPFlag("allowed-packages", rootCmd.Flags().Lookup("allowed-packages"))
	_ = viper.BindPFlag("enable-transports", rootCmd.Flags().Lookup("enable-transports"))
	_ = viper.BindPFlag("connection-read-only", rootCmd.Flags().Lookup("connection-read-only"))
	_ = viper.BindPFlag("allowed-transports", rootCmd.Flags().Lookup("allowed-transports"))
	_ = viper.BindPFlag("allow-transportable-edits", rootCmd.Flags().Lookup("allow-transportable-edits"))
	_ = viper.BindPFlag("mode", rootCmd.Flags().Lookup("mode"))
	_ = viper.BindPFlag("disabled-groups", rootCmd.Flags().Lookup("disabled-groups"))
	_ = viper.BindPFlag("verbose", rootCmd.Flags().Lookup("verbose"))

	// Feature configuration
	_ = viper.BindPFlag("feature-hana", rootCmd.Flags().Lookup("feature-hana"))
	_ = viper.BindPFlag("feature-abapgit", rootCmd.Flags().Lookup("feature-abapgit"))
	_ = viper.BindPFlag("feature-rap", rootCmd.Flags().Lookup("feature-rap"))
	_ = viper.BindPFlag("feature-amdp", rootCmd.Flags().Lookup("feature-amdp"))
	_ = viper.BindPFlag("feature-ui5", rootCmd.Flags().Lookup("feature-ui5"))
	_ = viper.BindPFlag("feature-connection", rootCmd.Flags().Lookup("feature-connection"))

	// Debugger configuration
	_ = viper.BindPFlag("terminal-id", rootCmd.Flags().Lookup("terminal-id"))

	// RFC connection settings
	_ = viper.BindPFlag("connection-mode", rootCmd.Flags().Lookup("connection-mode"))
	_ = viper.BindPFlag("ashost", rootCmd.Flags().Lookup("ashost"))
	_ = viper.BindPFlag("sysnr", rootCmd.Flags().Lookup("sysnr"))
	_ = viper.BindPFlag("mshost", rootCmd.Flags().Lookup("mshost"))
	_ = viper.BindPFlag("msserv", rootCmd.Flags().Lookup("msserv"))
	_ = viper.BindPFlag("r3name", rootCmd.Flags().Lookup("r3name"))
	_ = viper.BindPFlag("group", rootCmd.Flags().Lookup("group"))
	_ = viper.BindPFlag("jco-proxy-jar", rootCmd.Flags().Lookup("jco-proxy-jar"))
	_ = viper.BindPFlag("jco-libs-dir", rootCmd.Flags().Lookup("jco-libs-dir"))
	_ = viper.BindPFlag("java-path", rootCmd.Flags().Lookup("java-path"))
	_ = viper.BindPFlag("rfc-proxy-port", rootCmd.Flags().Lookup("rfc-proxy-port"))
	_ = viper.BindPFlag("rfc-max-concurrent", rootCmd.Flags().Lookup("rfc-max-concurrent"))

	// SNC/SSO configuration
	_ = viper.BindPFlag("snc", rootCmd.Flags().Lookup("snc"))
	_ = viper.BindPFlag("sysid", rootCmd.Flags().Lookup("sysid"))
	_ = viper.BindPFlag("landscape-file", rootCmd.Flags().Lookup("landscape-file"))

	// Set up environment variable mapping
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()
	viper.SetEnvPrefix("SAP")
}

func runServer(cmd *cobra.Command, _ []string) error {
	// Set verbose log output if requested (Viper/Cobra have already resolved this from CLI/ENV)
	if cfg.Verbose {
		adt.SetLogOutput(os.Stderr)
	}

	// Bootstrap the configuration: load config file, merge with CLI/ENV, augment, and validate
	// Viper/Cobra automatically handle CLI > ENV precedence, so no manual precedence checks needed
	bootstrappedCfg, err := internal.Bootstrap(cfg, singleSys, multiSystem, configFile, systemName, cmd)
	if err != nil {
		return err
	}

	if !multiSystem {
		// Copy cookie auth from env for single-system mode when flags are not provided.
		// Cookie loading itself is done in mcp.NewServer.
		if sys, ok := bootstrappedCfg.Systems[config.DefaultSystemID]; ok {
			if sys.CookieFile == "" {
				sys.CookieFile = viper.GetString("COOKIE_FILE")
			}
			if sys.CookieString == "" {
				sys.CookieString = viper.GetString("COOKIE_STRING")
			}
		}
	}

	// Shared mode: bridge this stdio session to a common background daemon
	// instead of opening an own SAP connection. Falls back to in-process serving
	// only if no daemon can be started.
	var shared *daemon.Daemon
	if sharedMode || sharedDaemonMode {
		key, err := sharedKey(bootstrappedCfg)
		if err != nil {
			return err
		}
		if sharedDaemonMode {
			shared, err = daemon.Listen(key, sharedIdleTimeout, daemon.Info{Version: Version, Systems: systemIDs(bootstrappedCfg)})
			if err != nil {
				return err
			}
		} else {
			err := runSharedShim(key)
			if !errors.Is(err, daemon.ErrSpawnFailed) {
				return err
			}
			_, _ = fmt.Fprintf(os.Stderr, "[VSP-SHARED] %v; serving this session in-process\n", err)
		}
	}
	// fail reports startup errors to shims waiting on the daemon.
	fail := func(err error) error {
		if shared != nil {
			shared.Fail(err)
			shared.Wait()
		}
		return err
	}

	// Handle browser-based SSO authentication (single-system mode only)
	// This must run after Bootstrap because it requires browser interaction
	if !multiSystem {
		if err := processBrowserAuthSingleSystem(cmd); err != nil {
			return fail(err)
		}
	}

	// Log final configuration if verbose
	if bootstrappedCfg.Verbose {
		logFinalConfiguration(bootstrappedCfg)
	}

	// Create and start the unified server (works for both single and multi-system)
	srv, err := mcp.NewServer(bootstrappedCfg, runtimeCookies)
	if err != nil {
		return fail(fmt.Errorf("server creation failed: %w", err))
	}
	defer func() {
		if err := srv.Shutdown(); err != nil && cfg.Verbose {
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Warning during shutdown: %v\n", err)
		}
	}()

	// Begin serving immediately. Connecting to SAP and starting the JCo sidecar
	// can take several seconds; doing that synchronously here would delay the MCP
	// protocol handshake and cause the client's first tool call to fail with an
	// undefined-reference error. Instead, register tools now and warm up each
	// system's connection in the background. Tool handlers block on the target
	// system's readiness (see System.EnsureReady) so the first call is slow but
	// succeeds.
	srv.ConnectAsync()

	if shared != nil {
		shared.Ready(srv.HTTPHandler())
		shared.Wait()
		return nil
	}

	return srv.ServeStdio()
}

// processBrowserAuthSingleSystem handles browser-based SSO authentication for single-system mode.
// This runs after Bootstrap and updates the config's single system with cookies.
func processBrowserAuthSingleSystem(cmd *cobra.Command) error {
	// Get the single system from cfg.Systems["default"]
	sys, ok := cfg.Systems[config.DefaultSystemID]
	if !ok {
		return fmt.Errorf("internal error: default system not found")
	}

	browserAuth, _ := cmd.Flags().GetBool("browser-auth")
	if !browserAuth && !viper.GetBool("BROWSER_AUTH") && !sys.BrowserAuth {
		return nil
	}

	if sys.URL == "" {
		return fmt.Errorf("--browser-auth requires --url to be set")
	}

	// Determine timeout
	timeout := 120 * time.Second
	if sys.BrowserAuthTimeout != "" {
		if d, err := time.ParseDuration(sys.BrowserAuthTimeout); err == nil {
			timeout = d
		}
	}
	if cmd.Flags().Changed("browser-auth-timeout") {
		timeout, _ = cmd.Flags().GetDuration("browser-auth-timeout")
	} else if v := viper.GetString("BROWSER_AUTH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	// Determine browser executable
	browserExec := sys.BrowserExec
	if cmd.Flags().Changed("browser-exec") {
		browserExec, _ = cmd.Flags().GetString("browser-exec")
	} else if v := viper.GetString("BROWSER_EXEC"); v != "" {
		browserExec = v
	}

	browserAuthURL := sys.BrowserAuthURL
	if cmd.Flags().Changed("browser-auth-url") {
		browserAuthURL, _ = cmd.Flags().GetString("browser-auth-url")
	} else if v := viper.GetString("BROWSER_AUTH_URL"); v != "" {
		browserAuthURL = v
	}

	if cfg.Verbose {
		_, _ = fmt.Fprintf(os.Stderr, "[BROWSER-AUTH] Starting browser login (%s)\n", sys.URL)
	}

	ctx := context.Background()
	cookies, err := adt.BrowserLoginWithTargetForClient(ctx, sys.URL, browserAuthURL, sys.Client, sys.Language, sys.Insecure, timeout, browserExec, cfg.Verbose)
	if err != nil {
		return fmt.Errorf("browser authentication failed: %w", err)
	}

	runtimeCookies[config.DefaultSystemID] = cookies

	// Save cookies to file if requested
	cookieSave := sys.CookieSave
	if cmd.Flags().Changed("cookie-save") {
		cookieSave, _ = cmd.Flags().GetString("cookie-save")
	} else if v := viper.GetString("COOKIE_SAVE"); v != "" {
		cookieSave = v
	}
	if cookieSave != "" {
		if err := adt.SaveCookiesToFile(cookies, sys.URL, cookieSave); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "[BROWSER-AUTH] Warning: failed to save cookies: %v\n", err)
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "[BROWSER-AUTH] Cookies saved to %s (reuse with --cookie-file)\n", cookieSave)
		}
	}

	return nil
}

// logFinalConfiguration logs the final configuration after augmentation and validation.
func logFinalConfiguration(cfg *config.GlobalConfig) {
	_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Starting vsp server\n")
	_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Mode: %s\n", cfg.Mode)

	if cfg.DisabledGroups != "" {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Disabled groups: %s (5/U=UI5, T=Tests, H=HANA, D=Debug)\n", cfg.DisabledGroups)
	}

	// Log per-system information
	for sysID, sys := range cfg.Systems {
		if strings.EqualFold(sys.ConnectionMode, "rfc") {
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] System %q: RFC mode (sidecar connection: stdio)\n", sysID)
			if sys.SNC {
				_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   Auth: SNC/SSO (system ID: %s, %d JCo properties)\n", sys.SysID, len(sys.JcoProperties))
			} else if sys.AsHost != "" {
				_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   RFC: Direct connection to %s (sysnr: %s)\n", sys.AsHost, sys.SysNr)
			} else if sys.MsHost != "" {
				_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   RFC: Load balanced via %s (r3name: %s, group: %s)\n", sys.MsHost, sys.R3Name, sys.Group)
			}
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] System %q: HTTP mode\n", sysID)
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   SAP URL: %s\n", sys.URL)
		}
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   Client: %s, Language: %s\n", sys.Client, sys.Language)
		if sys.Username != "" {
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   Auth: Basic (user: %s)\n", sys.Username)
		} else if sys.SNC {
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   Auth: SNC/SSO\n")
		} else if c := runtimeCookies[sysID]; len(c) > 0 {
			_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE]   Auth: Cookie (%d cookies)\n", len(c))
		}
	}

	// Log global settings
	if cfg.ReadOnly {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: READ-ONLY mode enabled\n")
	}
	if cfg.BlockFreeSQL {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: Free SQL queries BLOCKED\n")
	}
	if cfg.AllowedOps != "" {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: Allowed operations: %s\n", cfg.AllowedOps)
	}
	if cfg.DisallowedOps != "" {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: Disallowed operations: %s\n", cfg.DisallowedOps)
	}
	if len(cfg.AllowedPackages) > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: Allowed packages: %v\n", cfg.AllowedPackages)
	}
	if cfg.EnableTransports {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: Transport management ENABLED\n")
	}
	if cfg.AllowTransportableEdits {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: Transportable edits ENABLED (can modify non-local objects)\n")
	}
	if !cfg.ReadOnly && !cfg.BlockFreeSQL && cfg.AllowedOps == "" && cfg.DisallowedOps == "" && len(cfg.AllowedPackages) == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Safety: UNRESTRICTED (no safety checks active)\n")
	}
	if cfg.KeepAliveInterval > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "[VERBOSE] Session keep-alive: %s\n", cfg.KeepAliveInterval)
	}
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
