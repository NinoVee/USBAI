package apps

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ninovee/usbai/internal/config"
)

// TestCopyTreeLinks copies a tree with a link to a file, a link to a folder
// (as npm workspaces make), a link back to a parent (a loop) and a broken
// link: the copy has plain files and finishes.
func TestCopyTreeLinks(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "packages", "studio"), 0o755)
	os.WriteFile(filepath.Join(src, "packages", "studio", "index.js"), []byte("studio"), 0o644)
	os.MkdirAll(filepath.Join(src, "node_modules", ".bin"), 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	must := func(err error) {
		if err != nil {
			t.Skip("symbolic links not available:", err)
		}
	}
	must(os.Symlink(filepath.Join("..", "packages", "studio"), filepath.Join(src, "node_modules", "studio")))
	must(os.Symlink("a.txt", filepath.Join(src, "b.txt")))
	must(os.Symlink("..", filepath.Join(src, "packages", "studio", "up")))
	must(os.Symlink("missing", filepath.Join(src, "broken")))
	must(os.Symlink("../a.txt", filepath.Join(src, "node_modules", ".bin", "tool")))

	dst := filepath.Join(t.TempDir(), "out")
	skip := func(rel string) bool { return filepath.Base(rel) == ".bin" }
	if err := CopyTree(src, dst, skip); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"a.txt": "a", "b.txt": "a", "node_modules/studio/index.js": "studio", "packages/studio/index.js": "studio"} {
		b, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil || string(b) != want {
			t.Errorf("%s: %q %v", path, b, err)
		}
		if st, _ := os.Lstat(filepath.Join(dst, path)); st != nil && st.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is still a link", path)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules", ".bin")); err == nil {
		t.Error(".bin was copied")
	}
	if _, err := os.Lstat(filepath.Join(dst, "broken")); err == nil {
		t.Error("broken link was copied")
	}
}

// TestInstallerApp downloads this computer's installer and opens it; other
// computer kinds are told there is none for them.
func TestInstallerApp(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		t.Skip("opening runs the installer with the system's handler")
	}
	root := t.TempDir()
	marker := filepath.Join(root, "opened")
	script := "#!/bin/sh\necho yes > " + marker + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, script) }))
	defer srv.Close()
	cfg := config.Config{Root: root, Apps: []config.App{{ID: "fin", Name: "Fin", Kind: "installer",
		Downloads: map[string]string{"linux-x64": srv.URL + "/Fin-setup.run"}}}}

	other := NewManager(cfg, "macos-x64", io.Discard)
	if st := other.List(cfg.Apps)[0]; !strings.Contains(st.Missing, "no installer for macos-x64") {
		t.Errorf("other computer: %+v", st)
	}
	// A shared app installed earlier is seen as installed at start.
	os.MkdirAll(filepath.Join(root, "apps", "pack", "shared"), 0o755)
	os.WriteFile(filepath.Join(root, "apps", "pack", "shared", markerFile), []byte("{}"), 0o644)
	cfg.Apps = append(cfg.Apps, config.App{ID: "pack", Kind: "skills", Shared: true})
	m := NewManager(cfg, "linux-x64", io.Discard)
	if !m.Installed("pack") || m.List(cfg.Apps)[1].State != Installed {
		t.Error("shared app installed earlier not seen")
	}
	if err := m.OpenInstaller("fin"); err == nil {
		t.Error("opened before downloading")
	}
	if err := m.Install("fin"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !m.Installed("fin"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			log, _ := m.Log("fin")
			t.Fatalf("not downloaded:\n%s", log)
		}
	}
	if err := m.OpenInstaller("fin"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) == "yes" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("installer did not run")
		}
	}
}
