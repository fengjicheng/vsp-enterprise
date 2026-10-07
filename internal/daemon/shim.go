package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// ErrSpawnFailed means no daemon could be started. Nothing has been read from
// stdin at that point, so the caller can fall back to serving in-process.
var ErrSpawnFailed = errors.New("could not start shared vsp daemon")

// spawnTimeout bounds how long a freshly spawned daemon may take to bind its socket.
const spawnTimeout = 30 * time.Second

// ShimOptions configures RunShim.
type ShimOptions struct {
	Key     string
	Exe     string   // executable used to spawn the daemon
	Args    []string // daemon command-line arguments
	Dir     string   // daemon working directory
	Stdin   io.Reader
	Stdout  io.Writer
	Verbose bool
}

// startupError is a backend initialization failure reported by the daemon
// (e.g. a cancelled SSO login). Retrying would just repeat it.
type startupError struct{ msg string }

func (e *startupError) Error() string { return "shared vsp daemon failed to start: " + e.msg }

type shim struct {
	opts   ShimOptions
	paths  paths
	client *http.Client

	outMu sync.Mutex

	mu       sync.Mutex
	backend  *backend
	initReq  *transport.JSONRPCRequest // replayed after reconnecting to a new daemon
	initNote *mcp.JSONRPCNotification  // notifications/initialized, replayed likewise
	protoVer string                    // negotiated MCP protocol version
}

type backend struct {
	t            *transport.StreamableHTTP
	cancelAttach context.CancelFunc
	dead         chan struct{}
	deadOnce     sync.Once
}

func (b *backend) markDead() { b.deadOnce.Do(func() { close(b.dead) }) }

func (b *backend) isDead() bool {
	select {
	case <-b.dead:
		return true
	default:
		return false
	}
}

func (b *backend) close() {
	if b.t != nil {
		_ = b.t.Close()
	}
	b.cancelAttach()
}

// RunShim bridges MCP JSON-RPC between stdio and the shared daemon for opts.Key,
// spawning the daemon if necessary. It returns when stdin is closed.
func RunShim(ctx context.Context, opts ShimOptions) error {
	p, err := pathsFor(opts.Key)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSpawnFailed, err)
	}
	s := &shim{
		opts:  opts,
		paths: p,
		client: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", p.socket)
			},
		}},
	}

	// Connect before touching stdin so a spawn failure can still fall back.
	b, err := s.connect(ctx)
	if err != nil {
		return err
	}
	s.backend = b

	return s.serve(ctx)
}

func (s *shim) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "[VSP-SHARED] "+format+"\n", args...)
}

func (s *shim) debugf(format string, args ...any) {
	if s.opts.Verbose {
		s.logf(format, args...)
	}
}

func (s *shim) serve(ctx context.Context) error {
	r := bufio.NewReaderSize(s.opts.Stdin, 1<<20)
	var wg sync.WaitGroup
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.dispatch(ctx, line, &wg)
		}
		if err != nil {
			break
		}
	}
	wg.Wait()

	s.mu.Lock()
	if s.backend != nil {
		s.backend.close()
		s.backend = nil
	}
	s.mu.Unlock()
	return nil
}

func (s *shim) dispatch(ctx context.Context, line []byte, wg *sync.WaitGroup) {
	var env struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		s.logf("ignoring malformed message from client: %v", err)
		return
	}

	switch {
	case env.Method == "":
		// Response to a server-initiated request; vsp never issues any.
		return

	case len(env.ID) == 0 || string(env.ID) == "null":
		var n mcp.JSONRPCNotification
		if err := json.Unmarshal(line, &n); err != nil {
			s.logf("ignoring malformed notification: %v", err)
			return
		}
		if n.Method == "notifications/initialized" {
			s.mu.Lock()
			s.initNote = &n
			s.mu.Unlock()
		}
		b, err := s.getBackend(ctx)
		if err != nil {
			s.logf("dropping %s: %v", n.Method, err)
			return
		}
		if err := b.t.SendNotification(ctx, n); err != nil {
			s.debugf("sending %s failed: %v", n.Method, err)
		}

	default:
		req := transport.JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, Method: env.Method}
		if err := json.Unmarshal(env.ID, &req.ID); err != nil {
			s.logf("ignoring request with invalid id: %v", err)
			return
		}
		if len(env.Params) > 0 {
			req.Params = env.Params
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleRequest(ctx, req)
		}()
	}
}

func (s *shim) handleRequest(ctx context.Context, req transport.JSONRPCRequest) {
	b, err := s.getBackend(ctx)
	var resp *transport.JSONRPCResponse
	if err == nil {
		resp, err = b.t.SendRequest(ctx, req)
	}
	if err != nil {
		s.writeJSON(transport.JSONRPCResponse{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      req.ID,
			Error: &mcp.JSONRPCErrorDetails{
				Code:    mcp.INTERNAL_ERROR,
				Message: "vsp shared backend unavailable: " + err.Error(),
			},
		})
		return
	}

	if req.Method == string(mcp.MethodInitialize) && resp.Error == nil {
		ver := protocolVersion(resp.Result)
		b.t.SetProtocolVersion(ver)
		s.mu.Lock()
		s.initReq = &req
		s.protoVer = ver
		s.mu.Unlock()
	}
	s.writeJSON(resp)
}

// getBackend returns a live backend, reconnecting (and respawning the daemon)
// if the previous one went away. After reconnecting, the client's initialize
// handshake is replayed so the new session matches what the client negotiated.
func (s *shim) getBackend(ctx context.Context) (*backend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backend != nil && !s.backend.isDead() {
		return s.backend, nil
	}
	if s.backend != nil {
		s.backend.close()
		s.backend = nil
		s.logf("shared daemon went away, reconnecting")
	}

	b, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}

	if s.initReq != nil {
		if _, err := b.t.SendRequest(ctx, *s.initReq); err != nil {
			b.close()
			return nil, fmt.Errorf("replaying initialize: %w", err)
		}
		b.t.SetProtocolVersion(s.protoVer)
		if s.initNote != nil {
			if err := b.t.SendNotification(ctx, *s.initNote); err != nil {
				s.debugf("replaying initialized notification failed: %v", err)
			}
		}
	}

	s.backend = b
	return b, nil
}

// connect ensures a daemon is running and attaches to it, waiting until its
// backend is ready.
func (s *shim) connect(ctx context.Context) (*backend, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := s.ensureDaemon(ctx); err != nil {
			return nil, err
		}
		b, err := s.attach(ctx)
		if err == nil {
			return b, nil
		}
		var se *startupError
		if errors.As(err, &se) {
			return nil, err
		}
		// Typically a daemon that was just shutting down; spawn a fresh one.
		lastErr = err
		s.debugf("attach failed (attempt %d): %v", attempt+1, err)
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("attaching to shared vsp daemon: %w", lastErr)
}

func (s *shim) alive() bool {
	conn, err := net.DialTimeout("unix", s.paths.socket, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (s *shim) ensureDaemon(ctx context.Context) error {
	if s.alive() {
		return nil
	}

	unlock, err := lockFile(s.paths.lock)
	if err != nil {
		return fmt.Errorf("%w: locking %s: %v", ErrSpawnFailed, s.paths.lock, err)
	}
	defer unlock()

	// Another shim may have spawned it while we waited for the lock.
	if s.alive() {
		return nil
	}

	logFile, err := os.OpenFile(s.paths.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("%w: opening log %s: %v", ErrSpawnFailed, s.paths.log, err)
	}
	cmd := exec.Command(s.opts.Exe, s.opts.Args...)
	cmd.Dir = s.opts.Dir
	cmd.Env = os.Environ()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	started, err := startDetached(cmd, func(msg string) { s.logf("%s", msg) })
	_ = logFile.Close()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSpawnFailed, err)
	}
	s.logf("started shared daemon (pid %d), log: %s", started.Process.Pid, s.paths.log)

	exited := make(chan error, 1)
	go func() { exited <- started.Wait() }()

	deadline := time.NewTimer(spawnTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-exited:
			return fmt.Errorf("%w: daemon exited during startup (%v), see %s", ErrSpawnFailed, err, s.paths.log)
		case <-deadline.C:
			return fmt.Errorf("%w: daemon did not open %s within %s, see %s", ErrSpawnFailed, s.paths.socket, spawnTimeout, s.paths.log)
		case <-tick.C:
			if s.alive() {
				return nil
			}
		}
	}
}

// attach opens the long-lived /vsp/attach request. The daemon counts it as an
// active client until the connection closes, which also signals the shim when
// the daemon goes away.
func (s *shim) attach(ctx context.Context) (*backend, error) {
	attachCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(attachCtx, http.MethodGet, "http://vsp/vsp/attach", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	resp, err := s.client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("attach rejected: %s %s", resp.Status, strings.TrimSpace(string(body)))
	}

	// Blocks while the daemon connects to SAP (including any SSO login).
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("daemon closed connection during startup: %w", err)
	}
	if msg, ok := strings.CutPrefix(line, "error: "); ok {
		_ = resp.Body.Close()
		cancel()
		return nil, &startupError{msg: msg}
	}
	if line != "ready" {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("unexpected attach response %q", line)
	}

	b := &backend{cancelAttach: cancel, dead: make(chan struct{})}
	go func() {
		_, _ = io.Copy(io.Discard, br)
		_ = resp.Body.Close()
		b.markDead()
	}()

	t, err := transport.NewStreamableHTTP("http://vsp/mcp",
		transport.WithHTTPBasicClient(s.client),
		transport.WithContinuousListening(),
		transport.WithLogger(shimLogger{s}),
	)
	if err != nil {
		cancel()
		return nil, err
	}
	t.SetNotificationHandler(func(n mcp.JSONRPCNotification) { s.writeJSON(n) })
	if err := t.Start(context.Background()); err != nil {
		cancel()
		return nil, err
	}
	b.t = t
	return b, nil
}

func (s *shim) writeJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.logf("encoding message for client failed: %v", err)
		return
	}
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.opts.Stdout.Write(append(data, '\n'))
}

func protocolVersion(result json.RawMessage) string {
	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(result, &r)
	return r.ProtocolVersion
}

// shimLogger routes mcp-go transport logs to stderr, errors only unless verbose.
type shimLogger struct{ s *shim }

func (l shimLogger) Infof(format string, v ...any) { l.s.debugf("transport: "+format, v...) }

func (l shimLogger) Errorf(format string, v ...any) { l.s.debugf("transport error: "+format, v...) }
