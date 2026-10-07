package adt

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSidecarConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		config  SidecarConfig
		wantErr string
	}{
		{
			name:    "missing JcoProxyJar",
			config:  SidecarConfig{JcoLibsDir: "/tmp"},
			wantErr: "JcoProxyJar is required",
		},
		{
			name:    "JcoProxyJar not found",
			config:  SidecarConfig{JcoProxyJar: "/nonexistent/proxy.jar", JcoLibsDir: "/tmp"},
			wantErr: "JcoProxyJar not found",
		},
		{
			name:    "missing JcoLibsDir",
			config:  SidecarConfig{JcoProxyJar: createTempFile(t, "proxy.jar")},
			wantErr: "JcoLibsDir is required",
		},
		{
			name:    "JcoLibsDir not found",
			config:  SidecarConfig{JcoProxyJar: createTempFile(t, "proxy.jar"), JcoLibsDir: "/nonexistent/dir"},
			wantErr: "JcoLibsDir not found",
		},
		{
			name: "JcoLibsDir is file not dir",
			config: SidecarConfig{
				JcoProxyJar: createTempFile(t, "proxy.jar"),
				JcoLibsDir:  createTempFile(t, "notadir"),
			},
			wantErr: "JcoLibsDir is not a directory",
		},
		{
			name: "valid config",
			config: SidecarConfig{
				JcoProxyJar: createTempFile(t, "proxy.jar"),
				JcoLibsDir:  t.TempDir(),
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSidecarManager_BuildClasspath(t *testing.T) {
	// Create temp directory with some JARs
	libDir := t.TempDir()
	os.WriteFile(filepath.Join(libDir, "sapjco3.jar"), []byte("fake"), 0644)
	os.WriteFile(filepath.Join(libDir, "sapjco3-extra.jar"), []byte("fake"), 0644)
	os.WriteFile(filepath.Join(libDir, "native.so"), []byte("fake"), 0644) // not a JAR

	jarFile := createTempFile(t, "jco-proxy.jar")

	mgr := NewSidecarManager(&SidecarConfig{
		JcoProxyJar: jarFile,
		JcoLibsDir:  libDir,
	})

	cp := mgr.buildClasspath()

	// Should contain the proxy JAR
	if !strings.Contains(cp, jarFile) {
		t.Errorf("Classpath should contain proxy JAR %q, got: %s", jarFile, cp)
	}

	// Should contain the JCo JARs
	if !strings.Contains(cp, "sapjco3.jar") {
		t.Error("Classpath should contain sapjco3.jar")
	}
	if !strings.Contains(cp, "sapjco3-extra.jar") {
		t.Error("Classpath should contain sapjco3-extra.jar")
	}

	// Should NOT contain .so files
	if strings.Contains(cp, "native.so") {
		t.Error("Classpath should not contain native.so")
	}

	// Should use OS-appropriate path separator
	parts := strings.Split(cp, string(os.PathListSeparator))
	if len(parts) != 3 { // proxy.jar + 2 JCo JARs
		t.Errorf("Expected 3 classpath entries, got %d: %v", len(parts), parts)
	}
}

func TestSidecarManager_ParsePort(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		want   int
		wantOK bool
	}{
		{"valid port", "SIDECAR_PORT=8081", 8081, true},
		{"valid port with whitespace", "SIDECAR_PORT=9090 ", 9090, true},
		{"no prefix", "Server started on port 8081", 0, false},
		{"empty port", "SIDECAR_PORT=", 0, false},
		{"invalid port", "SIDECAR_PORT=abc", 0, false},
		{"zero port", "SIDECAR_PORT=0", 0, false},
		{"negative port", "SIDECAR_PORT=-1", 0, false},
		{"port too high", "SIDECAR_PORT=99999", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, ok := parsePortLine(tt.line)
			if ok != tt.wantOK {
				t.Errorf("parsePortLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if port != tt.want {
				t.Errorf("parsePortLine(%q) port = %v, want %v", tt.line, port, tt.want)
			}
		})
	}
}

func TestSidecarManager_BuildArgs(t *testing.T) {
	mgr := NewSidecarManager(&SidecarConfig{
		JcoProxyJar: "/opt/proxy.jar",
		JcoLibsDir:  "/opt/jcolibs",
		Port:        8081,
		AsHost:      "sap.example.com",
		SysNr:       "00",
		Client:      "001",
		Username:    "DEVELOPER",
		Password:    "secret123",
		Language:    "EN",
	})

	classpath := "/opt/proxy.jar:/opt/jcolibs/sapjco3.jar"
	args := mgr.buildArgs(classpath)

	// Check required args
	assertContainsArg(t, args, "-cp", classpath)
	assertContainsArg(t, args, "-Djava.library.path=/opt/jcolibs", "")
	assertContainsArg(t, args, "--ashost", "sap.example.com")
	assertContainsArg(t, args, "--sysnr", "00")
	assertContainsArg(t, args, "--client", "001")
	assertContainsArg(t, args, "--user", "DEVELOPER")
	assertContainsArg(t, args, "--password", "secret123")
	assertContainsArg(t, args, "--language", "EN")

	// Main class must be present
	found := false
	for _, arg := range args {
		if arg == "com.sap.mcp.proxy.RfcProxyServer" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Main class com.sap.mcp.proxy.RfcProxyServer not found in args")
	}
}

func TestSidecarManager_BuildArgs_LoadBalanced(t *testing.T) {
	mgr := NewSidecarManager(&SidecarConfig{
		JcoProxyJar: "/opt/proxy.jar",
		JcoLibsDir:  "/opt/jcolibs",
		MsHost:      "ms.example.com",
		MsServ:      "3600",
		R3Name:      "PRD",
		Group:       "PUBLIC",
		Client:      "100",
		Username:    "ADMIN",
		Password:    "pass",
	})

	classpath := "/opt/proxy.jar"
	args := mgr.buildArgs(classpath)

	assertContainsArg(t, args, "--mshost", "ms.example.com")
	assertContainsArg(t, args, "--msserv", "3600")
	assertContainsArg(t, args, "--r3name", "PRD")
	assertContainsArg(t, args, "--group", "PUBLIC")

	// Should NOT have --ashost or --sysnr
	for i, arg := range args {
		if arg == "--ashost" || arg == "--sysnr" {
			t.Errorf("Unexpected arg %q at position %d for load-balanced config", args[i], i)
		}
	}
}

func TestSidecarManager_BuildArgs_NoPort(t *testing.T) {
	mgr := NewSidecarManager(&SidecarConfig{
		JcoProxyJar: "/opt/proxy.jar",
		JcoLibsDir:  "/opt/jcolibs",
		Port:        0, // auto-assign
	})

	classpath := "/opt/proxy.jar"
	args := mgr.buildArgs(classpath)

	for _, arg := range args {
		if arg == "--port" {
			t.Error("Should not have --port arg when port is 0")
		}
	}
}

func TestSidecarManager_Defaults(t *testing.T) {
	mgr := NewSidecarManager(&SidecarConfig{
		JcoProxyJar: "/opt/proxy.jar",
		JcoLibsDir:  "/opt/jcolibs",
	})

	if mgr.config.JavaPath != "java" {
		t.Errorf("Default JavaPath = %v, want java", mgr.config.JavaPath)
	}
	if mgr.config.MaxConcurrent != 5 {
		t.Errorf("Default MaxConcurrent = %v, want 5", mgr.config.MaxConcurrent)
	}
}

// --- Helpers ---

func createTempFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	return path
}

func assertContainsArg(t *testing.T, args []string, key, value string) {
	t.Helper()
	for i, arg := range args {
		if arg == key {
			if value == "" {
				return // key-only match (e.g., -D flag)
			}
			if i+1 < len(args) && args[i+1] == value {
				return
			}
		}
		// Handle -Dfoo=bar style flags
		if value == "" && strings.HasPrefix(arg, key) {
			return
		}
	}
	if value == "" {
		t.Errorf("Arg %q not found in %v", key, args)
	} else {
		t.Errorf("Arg %q %q not found in %v", key, value, args)
	}
}

func TestParentPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps not available on Windows")
	}
	if got, want := parentPID(os.Getpid()), os.Getppid(); got != want {
		t.Errorf("parentPID(self) = %d, want %d", got, want)
	}
	if got := parentPID(-12345); got != -1 {
		t.Errorf("parentPID(invalid) = %d, want -1", got)
	}
}
