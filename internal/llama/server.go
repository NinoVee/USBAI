// Package llama starts and talks to llama.cpp's llama-server, which provides an
// OpenAI-compatible HTTP API on localhost.
package llama

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ninovee/usbai/internal/platform"
)

// Options configures one llama-server process.
type Options struct {
	RuntimeDir string // folder containing llama-server and its libraries
	Binary     string // llama-server or llama-server.exe
	Model      string // absolute path to the .gguf file
	Context    int
	GPULayers  int
	Threads    int
	Embedding  bool
	ExtraArgs  []string
	LogPrefix  string
	Log        io.Writer
}

// Server is a running llama-server process.
type Server struct {
	BaseURL string
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	mu      sync.Mutex
	tail    []string // last stderr lines, for error reports
}

// Start launches llama-server and waits until it reports healthy (the model
// is loaded) or exits. Loading a model from a USB drive can take a minute.
func Start(ctx context.Context, o Options) (*Server, error) {
	bin := filepath.Join(o.RuntimeDir, o.Binary)
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("runtime not found: %s", bin)
	}
	if runtime.GOOS != "windows" {
		// FAT32/exFAT drives can lose the executable bit; harmless if it fails.
		_ = os.Chmod(bin, 0o755)
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--model", o.Model,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--ctx-size", strconv.Itoa(max(o.Context, 512)),
		"--n-gpu-layers", strconv.Itoa(o.GPULayers),
	}
	if o.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(o.Threads))
	}
	if o.Embedding {
		// Embedding inputs must fit in one micro-batch.
		n := strconv.Itoa(max(o.Context, 512))
		args = append(args, "--embedding", "--batch-size", n, "--ubatch-size", n)
	} else {
		args = append(args, "--jinja")
	}
	args = append(args, o.ExtraArgs...)

	cmd := platform.HiddenCommand(bin, args...)
	cmd.Dir = o.RuntimeDir
	cmd.Env = withLibraryPath(os.Environ(), o.RuntimeDir)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	s := &Server{BaseURL: "http://127.0.0.1:" + strconv.Itoa(port), cmd: cmd, done: make(chan struct{})}
	go s.readLog(stderr, o)
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()

	if err := s.waitHealthy(ctx, 5*time.Minute); err != nil {
		s.Stop()
		return nil, err
	}
	return s, nil
}

func (s *Server) readLog(r io.Reader, o Options) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if o.Log != nil {
			fmt.Fprintf(o.Log, "[%s] %s\n", o.LogPrefix, line)
		}
		s.mu.Lock()
		s.tail = append(s.tail, line)
		if len(s.tail) > 20 {
			s.tail = s.tail[len(s.tail)-20:]
		}
		s.mu.Unlock()
	}
}

func (s *Server) lastLines() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.tail, "\n")
}

func (s *Server) waitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			return fmt.Errorf("llama-server exited: %v\n%s", s.waitErr, s.lastLines())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		resp, err := client.Get(s.BaseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return errors.New("timed out waiting for llama-server to load the model")
}

// Alive reports whether the process is still running.
func (s *Server) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Stop terminates the process.
func (s *Server) Stop() {
	if s == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// withLibraryPath makes shared libraries shipped next to llama-server
// loadable. Windows already searches the executable's folder for DLLs.
func withLibraryPath(env []string, dir string) []string {
	var key string
	switch runtime.GOOS {
	case "linux":
		key = "LD_LIBRARY_PATH"
	case "darwin":
		key = "DYLD_LIBRARY_PATH"
	default:
		return env
	}
	val := dir
	for i, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			if old := strings.TrimPrefix(kv, key+"="); old != "" {
				val = dir + string(os.PathListSeparator) + old
			}
			env[i] = key + "=" + val
			return env
		}
	}
	return append(env, key+"="+val)
}
