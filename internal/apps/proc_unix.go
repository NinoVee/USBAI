//go:build !windows

package apps

import (
	"context"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func command(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	startGroup(cmd)
	cmd.Cancel = func() error { killGroup(cmd); return nil }
	return cmd
}

// startGroup puts the app in its own process group, so stopping it also
// stops the browsers it started.
func startGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	go func() {
		time.Sleep(3 * time.Second)
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}()
}

// openFile opens an installer: macOS opens it with Finder (a .dmg is
// mounted), Linux runs it.
func openFile(p string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", p).Start()
	}
	return exec.Command(p).Start()
}
