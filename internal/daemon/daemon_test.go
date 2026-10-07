package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func setupDirs(t *testing.T) {
	t.Helper()
	// t.TempDir paths can exceed the Unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "vspd")
	if err != nil {
		t.Fatal(err)
	}
	baseDirOverride = dir
	t.Cleanup(func() {
		baseDirOverride = ""
		_ = os.RemoveAll(dir)
	})
}

func testKey(t *testing.T) string {
	return Key([]byte(t.Name()), []byte(fmt.Sprint(time.Now().UnixNano())))
}

func newTestMCP() *server.StreamableHTTPServer {
	s := server.NewMCPServer("test", "1.0.0")
	s.AddTool(mcp.NewTool("echo", mcp.WithString("text")), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("echo:" + req.GetString("text", "")), nil
	})
	return server.NewStreamableHTTPServer(s)
}

// testClient drives a shim over in-memory pipes like an MCP client would.
type testClient struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	done   chan error
	nextID int
}

func startShim(t *testing.T, key string) *testClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &testClient{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := RunShim(context.Background(), ShimOptions{
			Key:    key,
			Exe:    "/nonexistent/vsp",
			Stdin:  inR,
			Stdout: outW,
		})
		_ = outW.Close()
		c.done <- err
	}()
	return c
}

func (c *testClient) send(v map[string]any) {
	c.t.Helper()
	v["jsonrpc"] = "2.0"
	data, _ := json.Marshal(v)
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("writing to shim: %v", err)
	}
}

func (c *testClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"id": id, "method": method, "params": params})
	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("reading response to %s: %v", method, err)
		}
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			c.t.Fatalf("invalid JSON from shim: %s", line)
		}
		if msg["id"] == float64(id) {
			return msg
		}
	}
}

func (c *testClient) initialize() {
	c.t.Helper()
	resp := c.call("initialize", map[string]any{
		"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	if resp["error"] != nil {
		c.t.Fatalf("initialize failed: %v", resp["error"])
	}
	c.send(map[string]any{"method": "notifications/initialized"})
}

func (c *testClient) echo(text string) string {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"text": text}})
	if resp["error"] != nil {
		return fmt.Sprintf("error: %v", resp["error"])
	}
	content := resp["result"].(map[string]any)["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

func (c *testClient) close() error {
	_ = c.in.Close()
	select {
	case err := <-c.done:
		return err
	case <-time.After(10 * time.Second):
		c.t.Fatal("shim did not exit after stdin closed")
		return nil
	}
}

func clients(t *testing.T, key string) int {
	t.Helper()
	running, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range running {
		if r.Key == key {
			return r.Clients
		}
	}
	return -1
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSharedDaemonServesMultipleShims(t *testing.T) {
	setupDirs(t)
	key := testKey(t)

	d, err := Listen(key, 200*time.Millisecond, Info{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { d.Wait(); close(stopped) }()

	a := startShim(t, key)
	b := startShim(t, key)
	waitFor(t, "two attached clients", func() bool { return clients(t, key) == 2 })

	// Shims block in attach until the backend is ready.
	d.Ready(newTestMCP())
	a.initialize()
	b.initialize()

	if got := a.echo("a"); got != "echo:a" {
		t.Errorf("client a: got %q", got)
	}
	if got := b.echo("b"); got != "echo:b" {
		t.Errorf("client b: got %q", got)
	}

	if err := a.close(); err != nil {
		t.Errorf("shim a: %v", err)
	}
	waitFor(t, "one attached client", func() bool { return clients(t, key) == 1 })
	select {
	case <-stopped:
		t.Fatal("daemon stopped while a client was still attached")
	case <-time.After(400 * time.Millisecond):
	}

	if err := b.close(); err != nil {
		t.Errorf("shim b: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop after idle timeout")
	}
}

func TestShimReconnectsAndReplaysInitialize(t *testing.T) {
	setupDirs(t)
	key := testKey(t)

	d1, err := Listen(key, 0, Info{})
	if err != nil {
		t.Fatal(err)
	}
	d1.Ready(newTestMCP())

	c := startShim(t, key)
	c.initialize()
	if got := c.echo("one"); got != "echo:one" {
		t.Fatalf("before restart: got %q", got)
	}

	d1.Stop("test restart")
	d1.Wait()

	d2, err := Listen(key, 0, Info{})
	if err != nil {
		t.Fatal(err)
	}
	d2.Ready(newTestMCP())
	defer func() { d2.Stop("test done"); d2.Wait() }()

	// The next request transparently reconnects to the new daemon.
	if got := c.echo("two"); got != "echo:two" {
		t.Fatalf("after restart: got %q", got)
	}
	if err := c.close(); err != nil {
		t.Errorf("shim: %v", err)
	}
}

func TestShimReportsDaemonStartupFailure(t *testing.T) {
	setupDirs(t)
	key := testKey(t)

	d, err := Listen(key, 0, Info{})
	if err != nil {
		t.Fatal(err)
	}
	c := startShim(t, key)
	waitFor(t, "attached client", func() bool { return clients(t, key) == 1 })

	go d.Fail(errors.New("SSO login cancelled"))
	d.Wait()

	select {
	case err := <-c.done:
		var se *startupError
		if !errors.As(err, &se) || !strings.Contains(err.Error(), "SSO login cancelled") {
			t.Errorf("expected startup error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shim did not return after daemon startup failure")
	}
}

func TestShimSpawnFailureAllowsFallback(t *testing.T) {
	setupDirs(t)

	err := RunShim(context.Background(), ShimOptions{
		Key:    testKey(t),
		Exe:    "/nonexistent/vsp",
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
	})
	if !errors.Is(err, ErrSpawnFailed) {
		t.Fatalf("expected ErrSpawnFailed, got %v", err)
	}
}

func TestStopShutsDownDaemon(t *testing.T) {
	setupDirs(t)
	key := testKey(t)

	d, err := Listen(key, 0, Info{})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { d.Wait(); close(stopped) }()

	running, err := List()
	if err != nil || len(running) != 1 {
		t.Fatalf("List() = %v, %v; want one daemon", running, err)
	}
	if err := Stop(running[0], false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if running, _ := List(); len(running) != 0 {
		t.Errorf("daemon still listed after stop: %v", running)
	}
}
