// Command privateai is the drive launcher: it detects the host, starts the
// local model servers and opens the Private AI interface in the browser.
// Nothing listens on anything but 127.0.0.1 and nothing is sent off the computer.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/ninovee/usbai/internal/app"
	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/platform"
	"github.com/ninovee/usbai/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Private AI:", err)
		pause()
		os.Exit(1)
	}
}

func run() error {
	driveFlag := flag.String("drive", "", "drive root (default: found from the executable location)")
	portFlag := flag.Int("port", 0, "UI port (default from config.json)")
	noBrowser := flag.Bool("no-browser", false, "do not open a browser window")
	flag.Parse()

	root := *driveFlag
	if root == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		exe, _ = filepath.EvalSymlinks(exe)
		if root, err = config.FindRoot(filepath.Dir(exe)); err != nil {
			if root, err = config.FindRoot("."); err != nil {
				return err
			}
		}
	}
	cfg, err := config.Load(root)
	if err != nil {
		return err
	}
	if *portFlag != 0 {
		cfg.ListenPort = *portFlag
	}

	host := platform.Detect()
	fmt.Printf("PRIVATE AI %s — Offline • Local • Secure\n", version)
	fmt.Printf("Drive:    %s\n", root)
	fmt.Printf("Computer: %s, %d CPUs, %d GB RAM, accelerators %v\n", host.Slug(), host.CPUs, host.RAMGB(), host.Accel)

	// Prefer the configured port, but fall back to any free one so a second
	// drive (or a stale process) does not block startup.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.ListenPort))
	if err != nil {
		// If Private AI is already running (e.g. the launcher was
		// double-clicked twice), reuse it: a second copy would load the
		// models again and can run the computer out of memory.
		existing := fmt.Sprintf("http://127.0.0.1:%d/", cfg.ListenPort)
		if alreadyRunning(existing) {
			fmt.Printf("Private AI is already running. Opening %s\n", existing)
			if !*noBrowser {
				platform.OpenBrowser(existing)
			}
			return nil
		}
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	a := app.New(cfg, host, os.Stdout)
	a.Version = version
	a.StartEngines()
	srv := &http.Server{Handler: a.Handler(web.FS(), port), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "server:", err)
		}
	}()

	fmt.Printf("\nOpen %s in your browser.\nKeep this window open while you use Private AI. Press Ctrl+C or use Settings → Shut down before removing the drive.\n\n", url)
	if cfg.OpenBrowser && !*noBrowser {
		if err := platform.OpenBrowser(url); err != nil {
			fmt.Fprintln(os.Stderr, "could not open browser:", err)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-a.Shutdown:
		// Let the shutdown response reach the browser.
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Println("Shutting down… models unloaded, vault locked. It is now safe to remove the drive once this window closes.")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	a.Stop()
	return nil
}

func alreadyRunning(url string) bool {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(url + "api/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var st map[string]any
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return false
	}
	_, ok := st["chat_state"]
	return ok
}

// pause keeps a double-clicked console window open long enough to read an error.
func pause() {
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintln(os.Stderr, "Press Enter to close.")
		fmt.Scanln()
	}
}
