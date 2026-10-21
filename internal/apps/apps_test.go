package apps

import (
	"os"
	"path/filepath"
	"testing"
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
