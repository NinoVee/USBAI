//go:build windows

package apps

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
)

// createNoWindow keeps child processes from flashing a console window.
const createNoWindow = 0x08000000

func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	startGroup(cmd)
	return cmd
}

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	startGroup(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	return cmd
}

func startGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

// killGroup ends the app and every program it started.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if kill.Run() != nil {
		cmd.Process.Kill()
	}
}

// openFile runs an installer.
func openFile(p string) error { return exec.Command(p).Start() }
