package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/oisee/vibing-steampunk/embedded/deps"
	"github.com/oisee/vibing-steampunk/internal/config"
)

func TestNewToolResultError(t *testing.T) {
	result := newToolResultError("test error message")

	if result == nil {
		t.Fatal("newToolResultError returned nil")
	}

	if !result.IsError {
		t.Error("IsError should be true")
	}

	if len(result.Content) != 1 {
		t.Fatalf("Expected 1 content item, got %d", len(result.Content))
	}

	textContent, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("Content should be TextContent, got %T", result.Content[0])
	}

	if textContent.Text != "test error message" {
		t.Errorf("Text = %v, want 'test error message'", textContent.Text)
	}
}

func TestConfig(t *testing.T) {
	cfg := &config.SystemConfig{
		ConnectionConfig: config.ConnectionConfig{
			URL:      "https://sap.example.com:44300",
			Username: "testuser",
			Password: "testpass",
			Client:   "100",
			Language: "DE",
			Insecure: true,
		},
	}

	if cfg.URL != "https://sap.example.com:44300" {
		t.Errorf("URL = %v, want https://sap.example.com:44300", cfg.URL)
	}
	if cfg.Username != "testuser" {
		t.Errorf("User = %v, want testuser", cfg.Username)
	}
	if cfg.Password != "testpass" {
		t.Errorf("Password = %v, want testpass", cfg.Password)
	}
	if cfg.Client != "100" {
		t.Errorf("Client = %v, want 100", cfg.Client)
	}
	if cfg.Language != "DE" {
		t.Errorf("Language = %v, want DE", cfg.Language)
	}
	if !cfg.Insecure {
		t.Error("Insecure should be true")
	}
}

func TestNewServer(t *testing.T) {
	globalCfg := &config.GlobalConfig{}

	globalCfg.Systems = map[string]config.SystemConfig{
		config.DefaultSystemID: {
			ConnectionConfig: config.ConnectionConfig{
				URL:      "https://sap.example.com:44300",
				Username: "testuser",
				Password: "testpass",
				Client:   "001",
				Language: "EN",
			},
		},
	}

	srv, err := NewServer(globalCfg, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if srv.mcpServer == nil {
		t.Error("MCP server should not be nil")
	}
	if srv.router == nil {
		t.Fatal("Router should not be nil")
	}
	if len(srv.router.systems) != 1 {
		t.Fatalf("Expected 1 system in router, got %d", len(srv.router.systems))
	}
	// Verify the default system has a non-nil ADT client
	sys, ok := srv.router.systems["default"]

	if !ok {
		t.Fatal("Expected 'default' system in router")
	}
	if sys.ADT() == nil {
		t.Error("ADT client on default system should not be nil")
	}

	// Warm up in the background; connecting will fail due to the invalid URL.
	// The failure is surfaced through the target system's EnsureReady.
	ctx := context.Background()
	srv.ConnectAsync()
	if err := sys.EnsureReady(ctx); err == nil {
		t.Fatalf("EnsureReady should fail with invalid URL, but succeeded")
	}

	// Shutdown should work regardless of whether initialization succeeded
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("srv.Shutdown failed: %v", err)
	}
}

func TestResolveSystemCookiesUsesBrowserAuthAPIURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, `{"relatedUrls":{"API":"https://api.example.com","UI":%q}}`, serverURL(req))
	}))
	defer server.Close()

	sysCfg := config.SystemConfig{
		ConnectionConfig:  config.ConnectionConfig{URL: server.URL, Insecure: true},
		BrowserAuthConfig: config.BrowserAuthConfig{BrowserAuth: true},
	}
	cookies := map[string]map[string]string{"AED": {"SAP_SESSIONID_TEST": "session"}}
	got, err := resolveSystemCookies("AED", &sysCfg, false, cookies)
	if err != nil {
		t.Fatalf("resolveSystemCookies() failed: %v", err)
	}
	if got["SAP_SESSIONID_TEST"] != "session" {
		t.Fatalf("cookies = %#v", got)
	}
	if sysCfg.URL != "https://api.example.com" {
		t.Fatalf("system URL = %q, want API URL", sysCfg.URL)
	}
}

func serverURL(req *http.Request) string {
	return "https://" + req.Host
}

func TestDebuggerGetVariablesSchemaIncludesItems(t *testing.T) {
	globalCfg := &config.GlobalConfig{}

	globalCfg.Systems = map[string]config.SystemConfig{
		config.DefaultSystemID: {
			ConnectionConfig: config.ConnectionConfig{
				URL:      "https://sap.example.com:44300",
				Username: "testuser",
				Password: "testpass",
				Client:   "001",
				Language: "EN",
			},
		},
	}

	srv, err := NewServer(globalCfg, nil)
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	if srv == nil || srv.mcpServer == nil {
		t.Fatal("server or MCP server is nil")
	}

	// Register tools and warm up the connection in the background. Tool
	// registration is synchronous, so the schema is available for tools/list
	// even though connecting to the invalid URL will fail in the background.
	srv.ConnectAsync()
	defer func() {
		_ = srv.Shutdown()
	}()

	rawResponse := srv.mcpServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "tools/list",
		"params": {}
	}`))

	response, ok := rawResponse.(mcp.JSONRPCResponse)
	if !ok {
		t.Fatalf("expected JSONRPCResponse, got %T", rawResponse)
	}

	var tools []mcp.Tool
	switch result := response.Result.(type) {
	case mcp.ListToolsResult:
		tools = result.Tools
	case *mcp.ListToolsResult:
		tools = result.Tools
	default:
		t.Fatalf("expected ListToolsResult, got %T", response.Result)
	}

	var debuggerTool *mcp.Tool
	for i := range tools {
		if tools[i].Name == "DebuggerGetVariables" {
			debuggerTool = &tools[i]
			break
		}
	}
	if debuggerTool == nil {
		t.Fatal("DebuggerGetVariables tool not found")
	}

	variableIDsRaw, ok := debuggerTool.InputSchema.Properties["variable_ids"]
	if !ok {
		t.Fatal("variable_ids property not found in DebuggerGetVariables schema")
	}

	variableIDs, ok := variableIDsRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected variable_ids schema to be map[string]interface{}, got %T", variableIDsRaw)
	}

	if variableIDs["type"] != "array" {
		t.Fatalf("expected variable_ids type to be 'array', got %v", variableIDs["type"])
	}

	itemsRaw, ok := variableIDs["items"]
	if !ok {
		t.Fatal("variable_ids array schema is missing items")
	}

	items, ok := itemsRaw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected items to be map[string]interface{}, got %T", itemsRaw)
	}

	if items["type"] != "string" {
		t.Fatalf("expected variable_ids.items.type to be 'string', got %v", items["type"])
	}
}

func TestEnsureProxyJARIsAvailableDoesNotRewriteIdenticalJar(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.GlobalConfig{JcoLibsDir: dir}
	if err := ensureProxyJARIsAvailable(cfg); err != nil {
		t.Fatalf("first extraction failed: %v", err)
	}
	before, err := os.Stat(cfg.JcoProxyJar)
	if err != nil {
		t.Fatal(err)
	}

	cfg2 := &config.GlobalConfig{JcoLibsDir: dir}
	if err := ensureProxyJARIsAvailable(cfg2); err != nil {
		t.Fatalf("second extraction failed: %v", err)
	}
	after, err := os.Stat(cfg2.JcoProxyJar)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("identical proxy JAR was replaced; a running sidecar JVM would lose its file")
	}
}

func TestEnsureProxyJARIsAvailableReplacesStaleJarAtomically(t *testing.T) {
	dir := t.TempDir()
	proxyPath := filepath.Join(dir, "jco-proxy.jar")
	if err := os.WriteFile(proxyPath, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	// Simulate a running sidecar that holds the old JAR open.
	held, err := os.Open(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	cfg := &config.GlobalConfig{JcoLibsDir: dir}
	if err := ensureProxyJARIsAvailable(cfg); err != nil {
		t.Fatalf("extraction failed: %v", err)
	}

	got, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, deps.GetEmbeddedProxyJar()) {
		t.Error("stale proxy JAR was not replaced with embedded content")
	}
	old, err := io.ReadAll(held)
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "stale" {
		t.Errorf("open handle saw modified content %q; replacement was not atomic", old)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only jco-proxy.jar in %s, found %d entries", dir, len(entries))
	}
}
