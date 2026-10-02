// Command drivetool prepares a Private AI drive: it downloads llama.cpp
// runtimes for every supported platform and the GGUF models listed in
// config.json. It is used once, when building a drive, on a computer with
// internet access. The drive itself never goes online.
//
//	drivetool fetch-runtime -drive dist/PRIVATE-AI [-tag b6500] [-only linux-x64-cpu,macos-arm64]
//	drivetool fetch-models  -drive dist/PRIVATE-AI [-only qwen3-4b,nomic-embed]
//	drivetool check         -drive dist/PRIVATE-AI
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ninovee/usbai/internal/config"
)

// runtimeAsset maps a llama.cpp GitHub release asset onto a runtime folder.
type runtimeAsset struct {
	Folder  string
	Pattern *regexp.Regexp
	// Extra is a companion asset extracted into the same folder (the CUDA
	// runtime DLLs for the Windows CUDA build). $1 is the first capture
	// group of Pattern.
	Extra string
}

var runtimeAssets = []runtimeAsset{
	{Folder: "windows-x64-cuda", Pattern: regexp.MustCompile(`^llama-.*-bin-win-cuda-?(12\.[\d.]+)-x64\.zip$`), Extra: `^cudart-llama-bin-win-cuda-?$1-x64\.zip$`},
	{Folder: "windows-x64-vulkan", Pattern: regexp.MustCompile(`^llama-.*-bin-win-vulkan-x64\.zip$`)},
	{Folder: "windows-x64-cpu", Pattern: regexp.MustCompile(`^llama-.*-bin-win-cpu-x64\.zip$`)},
	{Folder: "windows-arm64-cpu", Pattern: regexp.MustCompile(`^llama-.*-bin-win-cpu-arm64\.zip$`)},
	{Folder: "macos-arm64", Pattern: regexp.MustCompile(`^llama-.*-bin-macos-arm64\.(zip|tar\.gz)$`)},
	{Folder: "macos-x64", Pattern: regexp.MustCompile(`^llama-.*-bin-macos-x64\.(zip|tar\.gz)$`)},
	{Folder: "linux-x64-vulkan", Pattern: regexp.MustCompile(`^llama-.*-bin-ubuntu-vulkan-x64\.(zip|tar\.gz)$`)},
	{Folder: "linux-x64-cpu", Pattern: regexp.MustCompile(`^llama-.*-bin-ubuntu-x64\.(zip|tar\.gz)$`)},
	{Folder: "linux-arm64-cpu", Pattern: regexp.MustCompile(`^llama-.*-bin-ubuntu-arm64\.(zip|tar\.gz)$`)},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	drive := fs.String("drive", "dist/PRIVATE-AI", "drive root containing config.json")
	only := fs.String("only", "", "comma-separated runtime folders or model ids to fetch (default: all)")
	tag := fs.String("tag", "latest", "llama.cpp release tag, e.g. b6500")
	fs.Parse(args)

	var err error
	switch cmd {
	case "fetch-runtime":
		err = fetchRuntime(*drive, *tag, splitList(*only))
	case "fetch-models":
		err = fetchModels(*drive, splitList(*only))
	case "check":
		err = check(*drive)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: drivetool fetch-runtime|fetch-models|check -drive DIR [-only a,b] [-tag bNNNN]")
	os.Exit(2)
}

func splitList(s string) map[string]bool {
	if s == "" {
		return nil
	}
	m := map[string]bool{}
	for _, x := range strings.Split(s, ",") {
		m[strings.TrimSpace(x)] = true
	}
	return m
}

// ---- Runtimes ----

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func fetchRuntime(drive, tag string, only map[string]bool) error {
	api := "https://api.github.com/repos/ggml-org/llama.cpp/releases/latest"
	if tag != "latest" {
		api = "https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/" + tag
	}
	resp, err := http.Get(api)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API: %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return err
	}
	fmt.Printf("llama.cpp release %s\n", rel.TagName)

	find := func(re *regexp.Regexp) (string, string, []string) {
		for _, a := range rel.Assets {
			if m := re.FindStringSubmatch(a.Name); m != nil {
				return a.Name, a.URL, m
			}
		}
		return "", "", nil
	}

	tmp, err := os.MkdirTemp("", "privateai-runtime")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	var missing []string
	for _, ra := range runtimeAssets {
		if only != nil && !only[ra.Folder] {
			continue
		}
		name, url, m := find(ra.Pattern)
		if name == "" {
			missing = append(missing, ra.Folder)
			continue
		}
		archives := []string{}
		p := filepath.Join(tmp, name)
		if err := download(url, p, ""); err != nil {
			return err
		}
		archives = append(archives, p)
		if ra.Extra != "" {
			extraRe := regexp.MustCompile(strings.ReplaceAll(ra.Extra, "$1", regexp.QuoteMeta(m[1])))
			if xname, xurl, _ := find(extraRe); xname != "" {
				xp := filepath.Join(tmp, xname)
				if err := download(xurl, xp, ""); err != nil {
					return err
				}
				archives = append(archives, xp)
			} else {
				fmt.Printf("  warning: companion archive for %s not found\n", ra.Folder)
			}
		}
		dest := filepath.Join(drive, "runtime", ra.Folder)
		if err := installRuntime(archives, dest, filepath.Join(tmp, ra.Folder)); err != nil {
			return fmt.Errorf("%s: %w", ra.Folder, err)
		}
		os.WriteFile(filepath.Join(dest, "VERSION.txt"), []byte("llama.cpp "+rel.TagName+"\n"), 0o644)
		fmt.Printf("  installed runtime/%s\n", ra.Folder)
	}
	if len(missing) > 0 {
		fmt.Printf("No matching release asset for: %s (asset names may have changed; edit runtimeAssets in drivetool)\n", strings.Join(missing, ", "))
	}
	return nil
}

// installRuntime extracts archives and copies llama-server plus the shared
// libraries it needs into dest, dereferencing symlinks (FAT32 and exFAT, the
// usual USB drive formats, cannot store them).
func installRuntime(archives []string, dest, work string) error {
	for _, a := range archives {
		if err := extract(a, work); err != nil {
			return err
		}
	}
	var serverDir string
	filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (d.Name() == "llama-server" || d.Name() == "llama-server.exe") {
			serverDir = filepath.Dir(p)
			return filepath.SkipAll
		}
		return nil
	})
	if serverDir == "" {
		return errors.New("llama-server not found in archive")
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	// The CUDA runtime DLLs may sit in a different folder of the work dir.
	dirs := map[string]bool{serverDir: true}
	filepath.WalkDir(work, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".dll") {
			dirs[filepath.Dir(p)] = true
		}
		return nil
	})
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !keepRuntimeFile(e.Name()) {
				continue
			}
			if err := copyFile(filepath.Join(dir, e.Name()), filepath.Join(dest, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func keepRuntimeFile(name string) bool {
	l := strings.ToLower(name)
	return l == "llama-server" || l == "llama-server.exe" ||
		strings.HasSuffix(l, ".dll") || strings.HasSuffix(l, ".dylib") || strings.HasSuffix(l, ".metal") ||
		strings.Contains(l, ".so") || strings.HasPrefix(l, "license")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // follows symlinks
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil || st.IsDir() {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm()|0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func extract(archive, dest string) error {
	if strings.HasSuffix(archive, ".zip") {
		return unzip(archive, dest)
	}
	return untargz(archive, dest)
}

func safeJoin(dest, name string) (string, error) {
	p := filepath.Join(dest, name)
	if !strings.HasPrefix(p, filepath.Clean(dest)+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe path in archive: %s", name)
	}
	return p, nil
}

func unzip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		p, err := safeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(p, 0o755)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		if f.Mode()&os.ModeSymlink != 0 {
			target, _ := io.ReadAll(rc)
			rc.Close()
			os.Remove(p)
			if err := os.Symlink(string(target), p); err != nil {
				return err
			}
			continue
		}
		out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode().Perm()|0o644)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func untargz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p, err := safeJoin(dest, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0o755)
		case tar.TypeSymlink:
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.Remove(p)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(p), 0o755)
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode).Perm()|0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			out.Close()
			if err != nil {
				return err
			}
		}
	}
}

// ---- Models ----

func fetchModels(drive string, only map[string]bool) error {
	cfg, err := config.Load(drive)
	if err != nil {
		return err
	}
	for _, m := range cfg.Models {
		if only != nil && !only[m.ID] {
			continue
		}
		if cfg.Present(m) {
			fmt.Printf("%s: already present\n", m.ID)
			continue
		}
		if m.URL == "" {
			fmt.Printf("%s: no url in config.json, skipping\n", m.ID)
			continue
		}
		fmt.Printf("%s: downloading %s\n", m.ID, m.Name)
		if err := download(m.URL, cfg.Path(m.File), m.SHA256); err != nil {
			return fmt.Errorf("%s: %w", m.ID, err)
		}
	}
	return nil
}

// download fetches url to path via a .part file, resuming a partial
// download, and verifies sha256 when given.
func download(url, path, sha string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	part := path + ".part"
	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	default:
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	total := offset + resp.ContentLength
	pw := &progress{name: filepath.Base(path), done: offset, total: total}
	_, err = io.Copy(io.MultiWriter(out, pw), resp.Body)
	out.Close()
	fmt.Println()
	if err != nil {
		return fmt.Errorf("download interrupted (re-run to resume): %w", err)
	}
	if sha != "" {
		if err := verify(part, sha); err != nil {
			os.Remove(part)
			return err
		}
	}
	return os.Rename(part, path)
}

func verify(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", path, got, want)
	}
	return nil
}

type progress struct {
	name        string
	done, total int64
	last        time.Time
}

func (p *progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) > 500*time.Millisecond {
		p.last = time.Now()
		if p.total > 0 {
			fmt.Printf("\r  %s: %d / %d MB (%.0f%%)   ", p.name, p.done>>20, p.total>>20, 100*float64(p.done)/float64(p.total))
		} else {
			fmt.Printf("\r  %s: %d MB   ", p.name, p.done>>20)
		}
	}
	return len(b), nil
}

// ---- Check ----

func check(drive string) error {
	cfg, err := config.Load(drive)
	if err != nil {
		return err
	}
	var total int64
	filepath.WalkDir(drive, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	fmt.Printf("Drive %s — %.1f GB used\n\nModels:\n", drive, float64(total)/(1<<30))
	for _, m := range cfg.Models {
		mark := "missing"
		if cfg.Present(m) {
			st, _ := os.Stat(cfg.Path(m.File))
			mark = fmt.Sprintf("ok (%.1f GB)", float64(st.Size())/(1<<30))
		}
		fmt.Printf("  %-12s %-10s %s\n", m.ID, m.Role, mark)
	}
	fmt.Println("\nRuntimes:")
	var folders []string
	for _, ra := range runtimeAssets {
		folders = append(folders, ra.Folder)
	}
	sort.Strings(folders)
	for _, f := range folders {
		mark := "missing"
		for _, bin := range []string{"llama-server", "llama-server.exe"} {
			if _, err := os.Stat(filepath.Join(drive, "runtime", f, bin)); err == nil {
				mark = "ok"
			}
		}
		fmt.Printf("  %-20s %s\n", f, mark)
	}
	fmt.Println("\nLaunchers:")
	for _, b := range []string{"windows-x64", "windows-arm64", "macos-arm64", "macos-x64", "linux-x64", "linux-arm64"} {
		exe := "privateai"
		if strings.HasPrefix(b, "windows") {
			exe += ".exe"
		}
		mark := "missing"
		if _, err := os.Stat(filepath.Join(drive, "bin", b, exe)); err == nil {
			mark = "ok"
		}
		fmt.Printf("  %-20s %s\n", b, mark)
	}
	return nil
}
