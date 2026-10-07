package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Running describes a daemon socket found in the runtime directory.
type Running struct {
	Info
	Socket string
	Err    error // set when the daemon did not answer
}

func unixClient(socket string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// List returns all daemons of the current user. Sockets nobody listens on
// anymore are removed.
func List() ([]Running, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return nil, err
	}
	sockets, err := filepath.Glob(filepath.Join(dir, "*.sock"))
	if err != nil {
		return nil, err
	}

	var result []Running
	for _, sock := range sockets {
		r := Running{Socket: sock}
		r.Key = strings.TrimSuffix(filepath.Base(sock), ".sock")

		resp, err := unixClient(sock, 3*time.Second).Get("http://vsp/vsp/status")
		if err != nil {
			if conn, dialErr := net.DialTimeout("unix", sock, time.Second); dialErr != nil {
				_ = os.Remove(sock) // stale
				continue
			} else {
				_ = conn.Close()
			}
			r.Err = err
			result = append(result, r)
			continue
		}
		err = json.NewDecoder(resp.Body).Decode(&r.Info)
		_ = resp.Body.Close()
		if err != nil {
			r.Err = err
		}
		result = append(result, r)
	}
	return result, nil
}

// Stop asks the daemon on r.Socket to shut down and waits until it has. With
// force, a daemon that does not stop in time is killed via its PID file.
func Stop(r Running, force bool) error {
	resp, err := unixClient(r.Socket, 3*time.Second).Post("http://vsp/vsp/shutdown", "text/plain", nil)
	if err == nil {
		_ = resp.Body.Close()
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", r.Socket, time.Second)
		if err != nil {
			return nil
		}
		_ = conn.Close()
		time.Sleep(200 * time.Millisecond)
	}

	if !force {
		return fmt.Errorf("daemon %s did not stop within 15s (use --force to kill it)", r.Key)
	}
	data, err := os.ReadFile(strings.TrimSuffix(r.Socket, ".sock") + ".pid")
	if err != nil {
		return fmt.Errorf("daemon %s: reading PID file: %w", r.Key, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("daemon %s: invalid PID file: %w", r.Key, err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("killing daemon %s (pid %d): %w", r.Key, pid, err)
	}
	_ = os.Remove(r.Socket)
	return nil
}
