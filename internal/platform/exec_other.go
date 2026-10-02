//go:build !windows

package platform

import "os/exec"

// HiddenCommand is exec.Command without a visible console window on Windows.
func HiddenCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func hiddenCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
