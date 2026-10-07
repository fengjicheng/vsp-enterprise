//go:build windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive, blocking lock on path. The returned func releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
		_ = f.Close()
	}, nil
}

// startDetached starts cmd without a visible console, in its own process group
// and, if the parent job allows it, outside the MCP client's job object. Clients
// such as VS Code put children into a kill-on-close job; without breakaway the
// daemon would die with the client that spawned it. If breakaway is refused the
// daemon still starts inside the job; other shims then respawn it when needed.
func startDetached(cmd *exec.Cmd, warn func(string)) (*exec.Cmd, error) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: flags | windows.CREATE_BREAKAWAY_FROM_JOB,
	}
	err := cmd.Start()
	if err == nil {
		return cmd, nil
	}
	warn("could not break away from the client's job object (" + err.Error() + "); the daemon will stop when this client exits")

	retry := exec.Command(cmd.Path, cmd.Args[1:]...)
	retry.Dir, retry.Env = cmd.Dir, cmd.Env
	retry.Stdin, retry.Stdout, retry.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
	retry.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	if err := retry.Start(); err != nil {
		return nil, err
	}
	return retry, nil
}
