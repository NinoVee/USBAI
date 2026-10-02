//go:build windows

package platform

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps child processes from flashing a console window.
const createNoWindow = 0x08000000

// HiddenCommand is exec.Command without a visible console window on Windows.
func HiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

func hiddenCommand(name string, args ...string) *exec.Cmd { return HiddenCommand(name, args...) }
