//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

// lockFile takes an exclusive, blocking lock on path. The returned func releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// startDetached starts cmd in its own session so it survives the MCP client
// killing the shim's process group. It returns the started command.
func startDetached(cmd *exec.Cmd, _ func(string)) (*exec.Cmd, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
