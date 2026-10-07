// Package daemon lets several MCP clients share one vsp backend process.
//
// Each MCP client starts vsp over stdio as usual. With --shared, that process acts
// as a thin shim: it locates (or spawns) a background daemon for the same effective
// configuration and bridges its stdio to the daemon's Streamable HTTP endpoint,
// served on a per-user Unix domain socket. All clients therefore share one SAP
// session, one JCo sidecar and one browser SSO login.
//
// Lifecycle:
//   - The daemon is keyed by a hash of binary + effective configuration. A client
//     with a different config or binary gets its own daemon.
//   - Every shim holds a long-lived /vsp/attach request. When the last one goes
//     away, the daemon shuts down after the idle timeout.
//   - "vsp daemon stop" shuts daemons down explicitly; connected shims respawn a
//     fresh daemon on their next request.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// maxSocketPath stays below the sun_path limit (104 on macOS, 108 elsewhere).
const maxSocketPath = 100

// Key derives a short, stable daemon identifier from the given parts.
func Key(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(p))
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// baseDirOverride replaces all per-user directories (tests only).
var baseDirOverride string

// RuntimeDir returns the per-user directory holding daemon sockets, locks and PID files.
func RuntimeDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "vsp", "daemon")
	if baseDirOverride != "" {
		dir = filepath.Join(baseDirOverride, "daemon")
	}
	if len(filepath.Join(dir, "0123456789abcdef.sock")) > maxSocketPath {
		// Very long home paths would exceed the socket path limit.
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("vsp-daemon-%d", os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating daemon directory %s: %w", dir, err)
	}
	return dir, nil
}

// LogDir returns the per-user directory for daemon log files.
func LogDir() (string, error) {
	var dir string
	if baseDirOverride != "" {
		dir = filepath.Join(baseDirOverride, "logs")
	} else if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "Library", "Logs", "vsp")
		}
	}
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "vsp", "logs")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("creating log directory %s: %w", dir, err)
	}
	return dir, nil
}

type paths struct {
	socket string
	lock   string
	pid    string
	log    string
}

func pathsFor(key string) (paths, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return paths{}, err
	}
	logDir, err := LogDir()
	if err != nil {
		return paths{}, err
	}
	return paths{
		socket: filepath.Join(dir, key+".sock"),
		lock:   filepath.Join(dir, key+".lock"),
		pid:    filepath.Join(dir, key+".pid"),
		log:    filepath.Join(logDir, "daemon-"+key+".log"),
	}, nil
}
