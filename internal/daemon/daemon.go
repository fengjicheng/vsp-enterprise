package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info describes a running daemon; it is served on /vsp/status.
type Info struct {
	Key     string    `json:"key"`
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Systems []string  `json:"systems"`
	Dir     string    `json:"dir"`
	Started time.Time `json:"started"`
	Clients int       `json:"clients"`
	Ready   bool      `json:"ready"`
}

// Daemon serves the shared MCP backend on a Unix domain socket.
//
// It listens immediately so shims can attach while SAP connection setup (and a
// possible browser SSO login) is still in progress; their attach requests block
// until Ready or Fail is called.
type Daemon struct {
	info        Info
	paths       paths
	idleTimeout time.Duration
	log         func(format string, args ...any)

	httpSrv    *http.Server
	baseCtx    context.Context // parent of all request contexts, cancelled on shutdown
	cancelBase context.CancelFunc

	mu        sync.Mutex
	clients   int
	idleTimer *time.Timer
	mcp       http.Handler
	initErr   error

	settled  chan struct{} // closed by Ready or Fail
	stopping chan struct{} // closed when shutdown is requested
	stopOnce sync.Once
}

// Listen binds the daemon socket for key and starts serving control endpoints.
// idleTimeout <= 0 keeps the daemon running until stopped explicitly.
func Listen(key string, idleTimeout time.Duration, info Info) (*Daemon, error) {
	p, err := pathsFor(key)
	if err != nil {
		return nil, err
	}

	// A leftover socket file from a crashed daemon blocks bind. The spawning shim
	// holds the key lock and only spawns when nothing answers on the socket.
	_ = os.Remove(p.socket)
	ln, err := net.Listen("unix", p.socket)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", p.socket, err)
	}
	_ = os.Chmod(p.socket, 0600)

	info.Key = key
	info.PID = os.Getpid()
	info.Started = time.Now()
	if info.Dir == "" {
		info.Dir, _ = os.Getwd()
	}

	d := &Daemon{
		info:        info,
		paths:       p,
		idleTimeout: idleTimeout,
		log: func(format string, args ...any) {
			_, _ = fmt.Fprintf(os.Stderr, "%s [DAEMON] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
		},
		settled:  make(chan struct{}),
		stopping: make(chan struct{}),
	}

	_ = os.WriteFile(p.pid, []byte(strconv.Itoa(info.PID)), 0600)

	mux := http.NewServeMux()
	mux.HandleFunc("/vsp/attach", d.handleAttach)
	mux.HandleFunc("/vsp/status", d.handleStatus)
	mux.HandleFunc("/vsp/shutdown", d.handleShutdown)
	mux.HandleFunc("/mcp", d.handleMCP)
	d.baseCtx, d.cancelBase = context.WithCancel(context.Background())
	d.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return d.baseCtx },
	}

	// Exit if no client attaches at all.
	d.armIdleTimer()

	go func() {
		if err := d.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log("serve error: %v", err)
			d.Stop("serve error")
		}
	}()

	d.log("listening on %s (pid %d, idle timeout %s)", p.socket, info.PID, idleTimeout)
	return d, nil
}

// Ready installs the MCP handler and releases waiting shims.
func (d *Daemon) Ready(mcp http.Handler) {
	d.mu.Lock()
	d.mcp = mcp
	d.mu.Unlock()
	close(d.settled)
	d.log("backend ready")
}

// Fail reports a startup error to waiting shims and stops the daemon.
func (d *Daemon) Fail(err error) {
	d.mu.Lock()
	d.initErr = err
	d.mu.Unlock()
	close(d.settled)
	d.log("backend startup failed: %v", err)
	d.Stop("startup failed")
}

// Stop requests shutdown; Wait returns once the daemon has stopped serving.
func (d *Daemon) Stop(reason string) {
	d.stopOnce.Do(func() {
		d.log("shutting down: %s", reason)
		close(d.stopping)
	})
}

// Wait blocks until Stop is called, then closes the socket and cleans up.
func (d *Daemon) Wait() {
	<-d.stopping

	// Cancelling the base context ends attach requests and long-lived MCP listen
	// streams (and aborts in-flight tool calls), so Shutdown completes promptly.
	d.cancelBase()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.httpSrv.Shutdown(ctx); err != nil {
		_ = d.httpSrv.Close()
	}

	if data, err := os.ReadFile(d.paths.pid); err == nil && strings.TrimSpace(string(data)) == strconv.Itoa(d.info.PID) {
		_ = os.Remove(d.paths.pid)
	}
	d.log("stopped")
}

func (d *Daemon) armIdleTimer() {
	if d.idleTimeout <= 0 {
		return
	}
	d.idleTimer = time.AfterFunc(d.idleTimeout, func() {
		d.mu.Lock()
		idle := d.clients == 0
		d.mu.Unlock()
		if idle {
			d.Stop(fmt.Sprintf("no clients for %s", d.idleTimeout))
		}
	})
}

func (d *Daemon) attach() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clients++
	if d.idleTimer != nil {
		d.idleTimer.Stop()
	}
	return d.clients
}

func (d *Daemon) detach() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clients--
	if d.clients == 0 && d.idleTimer != nil {
		d.idleTimer.Reset(d.idleTimeout)
	}
	return d.clients
}

// handleAttach registers a client for the lifetime of the request. It answers
// "ready" or "error: ..." once the backend has settled, then holds the
// connection open; the client disconnecting (or dying) detaches it.
func (d *Daemon) handleAttach(w http.ResponseWriter, r *http.Request) {
	select {
	case <-d.stopping:
		http.Error(w, "daemon is shutting down", http.StatusServiceUnavailable)
		return
	default:
	}

	d.log("client attached (%d active)", d.attach())
	defer func() { d.log("client detached (%d active)", d.detach()) }()

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	select {
	case <-d.settled:
	case <-r.Context().Done():
		// Fail settles before shutting down; still deliver its error.
		select {
		case <-d.settled:
		default:
			return
		}
	}

	d.mu.Lock()
	initErr := d.initErr
	d.mu.Unlock()
	if initErr != nil {
		_, _ = fmt.Fprintf(w, "error: %s\n", strings.ReplaceAll(initErr.Error(), "\n", " "))
		return
	}

	_, _ = fmt.Fprintln(w, "ready")
	if flusher != nil {
		flusher.Flush()
	}

	<-r.Context().Done()
}

func (d *Daemon) handleStatus(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	info := d.info
	info.Clients = d.clients
	info.Ready = d.mcp != nil
	d.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	go d.Stop("requested via vsp daemon stop")
}

func (d *Daemon) handleMCP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	h := d.mcp
	d.mu.Unlock()
	if h == nil {
		http.Error(w, "backend not ready", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}
