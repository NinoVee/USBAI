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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
	template := fs.String("template", "drive/config.json", "config template for sync-config")
	fs.Parse(args)

	var err error
	switch cmd {
	case "fetch-runtime":
		err = fetchRuntime(*drive, *tag, splitList(*only))
	case "fetch-models":
		err = fetchModels(*drive, splitList(*only))
	case "check":
		err = check(*drive)
	case "verify":
		err = verifyDrive(*drive)
	case "sync-config":
		err = syncConfig(*drive, *template)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: drivetool fetch-runtime|fetch-models|sync-config|check|verify -drive DIR [-only a,b] [-tag bNNNN]")
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

type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

func fetchRuntime(drive, tag string, only map[string]bool) error {
	rel, err := pickRelease(tag, only)
	if err != nil {
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

// pickRelease chooses the llama.cpp release to install. Prebuilt binaries
// are published on numbered build tags (bNNNN). GitHub's "latest" release can
// be a vX.Y.Z release without them, and the newest build's binaries may still
// be uploading, so for "latest" this walks back through recent build tags
// until one has every wanted runtime.
func pickRelease(tag string, only map[string]bool) (release, error) {
	var cands []release
	if tag == "latest" {
		var err error
		if cands, err = recentBuilds(10); err != nil {
			return release{}, err
		}
	} else {
		rel, err := releaseFromAPI(tag)
		if err != nil {
			rel = release{TagName: tag}
		}
		cands = []release{rel}
	}

	var best release
	bestGot := 0
	for _, rel := range cands {
		if len(rel.Assets) == 0 {
			rel = probeAssets(rel.TagName)
		}
		got, want := coverage(rel, only)
		if got == want && want > 0 {
			return rel, nil
		}
		fmt.Printf("  %s has %d of %d runtimes; trying an older build\n", rel.TagName, got, want)
		if got > bestGot {
			best, bestGot = rel, got
		}
	}
	if bestGot > 0 {
		return best, nil
	}
	return release{}, errors.New("no llama.cpp release with downloadable runtimes found; pass -tag bNNNN")
}

// coverage counts the wanted runtime folders that have a matching asset.
func coverage(rel release, only map[string]bool) (got, want int) {
	for _, ra := range runtimeAssets {
		if only != nil && !only[ra.Folder] {
			continue
		}
		want++
		for _, a := range rel.Assets {
			if ra.Pattern.MatchString(a.Name) {
				got++
				break
			}
		}
	}
	return got, want
}

// recentBuilds returns the newest bNNNN releases, newest first. It uses the
// GitHub API (which includes asset lists) and falls back to git tags when the
// API is blocked or rate-limited.
func recentBuilds(n int) ([]release, error) {
	var out []release
	resp, err := http.Get("https://api.github.com/repos/ggml-org/llama.cpp/releases?per_page=30")
	if err == nil {
		var rels []release
		if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&rels) == nil {
			for _, r := range rels {
				if buildNumber(r.TagName) > 0 {
					out = append(out, r)
				}
			}
		}
		resp.Body.Close()
	}
	if len(out) == 0 {
		fmt.Println("GitHub API unavailable; listing llama.cpp builds with git")
		gitOut, err := exec.Command("git", "ls-remote", "--tags", "https://github.com/ggml-org/llama.cpp", "refs/tags/b*").Output()
		if err != nil {
			return nil, fmt.Errorf("could not list llama.cpp tags (is git installed?): %w; pass -tag bNNNN", err)
		}
		for _, line := range strings.Split(string(gitOut), "\n") {
			if tag := line[strings.LastIndex(line, "/")+1:]; buildNumber(tag) > 0 {
				out = append(out, release{TagName: tag})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return buildNumber(out[i].TagName) > buildNumber(out[j].TagName) })
	if len(out) > n {
		out = out[:n]
	}
	if len(out) == 0 {
		return nil, errors.New("no llama.cpp build tags found; pass -tag bNNNN")
	}
	return out, nil
}

// buildNumber returns NNNN for a "bNNNN" tag, or 0.
func buildNumber(tag string) int {
	if !strings.HasPrefix(tag, "b") || strings.HasSuffix(tag, "^{}") {
		return 0
	}
	n, err := strconv.Atoi(tag[1:])
	if err != nil {
		return 0
	}
	return n
}

func releaseFromAPI(tag string) (release, error) {
	var rel release
	resp, err := http.Get("https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/" + tag)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("GitHub API: %s", resp.Status)
	}
	err = json.NewDecoder(resp.Body).Decode(&rel)
	return rel, err
}

// probeAssets finds which known asset names exist for tag without the API.
func probeAssets(tag string) release {
	rel := release{TagName: tag}
	var names []string
	for _, p := range []string{"ubuntu-x64", "ubuntu-vulkan-x64", "ubuntu-arm64", "macos-arm64", "macos-x64", "win-cpu-x64", "win-cpu-arm64", "win-vulkan-x64"} {
		for _, ext := range []string{".tar.gz", ".zip"} {
			names = append(names, "llama-"+tag+"-bin-"+p+ext)
		}
	}
	for _, v := range []string{"12.4", "12.6", "12.8", "12.9"} {
		names = append(names, "llama-"+tag+"-bin-win-cuda-"+v+"-x64.zip", "cudart-llama-bin-win-cuda-"+v+"-x64.zip")
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := "https://github.com/ggml-org/llama.cpp/releases/download/" + tag + "/"
	for _, n := range names {
		resp, err := client.Head(base + n)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusFound {
			rel.Assets = append(rel.Assets, asset{Name: n, URL: base + n})
		}
	}
	return rel
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
		if cfg.CheckFile(m.File, m.Size) == nil {
			fmt.Printf("%s: already present\n", m.ID)
		} else if m.URL == "" {
			fmt.Printf("%s: no url in config.json, skipping\n", m.ID)
			continue
		} else {
			if cfg.Present(m) {
				fmt.Printf("%s: %v — downloading again\n", m.ID, cfg.Check(m))
			}
			fmt.Printf("%s: downloading %s\n", m.ID, m.Name)
			if err := download(m.URL, cfg.Path(m.File), m.SHA256); err != nil {
				return fmt.Errorf("%s: %w", m.ID, err)
			}
		}
		// Vision and speech models also need their projector (image or
		// audio encoder).
		if m.MMProj != "" && m.MMProjURL != "" && !cfg.Vision(m) {
			part := "image projector"
			if m.Role == "speech" {
				part = "audio encoder"
			}
			fmt.Printf("%s: downloading %s\n", m.ID, part)
			if err := download(m.MMProjURL, cfg.Path(m.MMProj), m.MMProjSHA256); err != nil {
				return fmt.Errorf("%s mmproj: %w", m.ID, err)
			}
		}
		for _, x := range m.Extra {
			if cfg.CheckFile(x.File, x.Size) == nil || x.URL == "" {
				continue
			}
			fmt.Printf("%s: downloading %s\n", m.ID, filepath.Base(x.File))
			var err error
			if x.Member != "" {
				err = downloadMember(x.URL, x.Member, cfg.Path(x.File), x.SHA256)
			} else {
				err = download(x.URL, cfg.Path(x.File), x.SHA256)
			}
			if err != nil {
				return fmt.Errorf("%s %s: %w", m.ID, x.File, err)
			}
		}
	}
	fmt.Println("All models are downloaded and verified.")
	return nil
}

// download fetches url to path, retrying dropped connections: each retry
// resumes from the partial .part file, so nothing is downloaded twice.
func download(url, path, sha string) error {
	var err error
	for attempt := 1; attempt <= 6; attempt++ {
		if err = downloadOnce(url, path, sha); err == nil || !retryable(err) {
			return err
		}
		wait := time.Duration(attempt*attempt) * 2 * time.Second
		fmt.Printf("  connection dropped (%v); resuming in %s (attempt %d of 6)\n", err, wait, attempt+1)
		time.Sleep(wait)
	}
	return err
}

// retryable reports network failures worth resuming after.
func retryable(err error) bool {
	msg := err.Error()
	for _, s := range []string{"interrupted", "connection reset", "timeout", "EOF", "broken pipe", "no such host", "connection refused", "502", "503", "504"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// downloadOnce fetches url to path via a .part file, resuming a partial
// download, and verifies sha256 when given.
func downloadOnce(url, path, sha string) error {
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
	pw.print() // the final count, even for files that finish within a tick
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

// downloadMember fetches a .tgz archive and saves one file from it.
func downloadMember(url, member, path, sha string) error {
	tgz := path + ".tgz"
	if err := download(url, tgz, ""); err != nil {
		return err
	}
	defer os.Remove(tgz)
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not found in %s", member, url)
		}
		if err != nil {
			return err
		}
		if h.Name != member {
			continue
		}
		part := path + ".part"
		out, err := os.Create(part)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, 1<<30))
		out.Close()
		if err == nil && sha != "" {
			err = verify(part, sha)
		}
		if err != nil {
			os.Remove(part)
			return err
		}
		return os.Rename(part, path)
	}
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
		p.print()
	}
	return len(b), nil
}

func (p *progress) print() {
	if p.total > 0 {
		fmt.Printf("\r  %s: %s / %s (%.0f%%)   ", p.name, humanSize(p.done), humanSize(p.total), 100*float64(p.done)/float64(p.total))
	} else {
		fmt.Printf("\r  %s: %s   ", p.name, humanSize(p.done))
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// ---- Config sync ----

// syncConfig adds models from the template that an existing drive's
// config.json lacks (e.g. after an update adds vision models), without
// touching the user's other settings or their own model entries.
func syncConfig(drive, template string) error {
	path := filepath.Join(drive, config.FileName)
	cur, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tmpl, err := os.ReadFile(template)
	if err != nil {
		return err
	}
	var curMap map[string]json.RawMessage
	if err := json.Unmarshal(cur, &curMap); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var curModels, tmplModels []map[string]any
	json.Unmarshal(curMap["models"], &curModels)
	var t struct {
		Models []map[string]any `json:"models"`
		// Retired models are removed from drives by updates.
		Retired []string `json:"retired_models"`
	}
	if err := json.Unmarshal(tmpl, &t); err != nil {
		return fmt.Errorf("%s: %w", template, err)
	}
	tmplModels = t.Models

	byID := map[string]map[string]any{}
	for _, m := range curModels {
		byID[fmt.Sprint(m["id"])] = m
	}
	// Template order first (it encodes model preference), keeping the
	// user's version of each existing entry; then the user's own models.
	var merged []map[string]any
	seen := map[string]bool{}
	var added, updated []string
	for _, m := range tmplModels {
		id := fmt.Sprint(m["id"])
		seen[id] = true
		if old, ok := byID[id]; ok && revision(m) > revision(old) {
			// The template's entry was revised: it replaces the drive's.
			merged = append(merged, m)
			updated = append(updated, id)
		} else if ok {
			// Keep the user's entry but pick up newly introduced fields.
			for k, v := range m {
				if _, has := old[k]; !has {
					old[k] = v
				}
			}
			merged = append(merged, old)
		} else {
			merged = append(merged, m)
			added = append(added, id)
		}
	}
	retired := map[string]bool{}
	for _, id := range t.Retired {
		retired[id] = true
	}
	var removed []string
	var oldFiles []string
	for _, m := range curModels {
		id := fmt.Sprint(m["id"])
		switch {
		case seen[id]:
		case retired[id]:
			removed = append(removed, id)
			oldFiles = append(oldFiles, modelFiles(m)...)
		default:
			merged = append(merged, m)
		}
	}
	if len(added) == 0 && len(removed) == 0 && len(updated) == 0 {
		fmt.Println("config.json already has every model")
	}
	b, _ := json.Marshal(merged)
	curMap["models"] = b
	out, err := json.MarshalIndent(curMap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return err
	}
	if len(added) > 0 {
		fmt.Printf("config.json: added %s\n", strings.Join(added, ", "))
	}
	if len(updated) > 0 {
		fmt.Printf("config.json: updated %s\n", strings.Join(updated, ", "))
	}
	if len(removed) > 0 {
		fmt.Printf("config.json: removed %s\n", strings.Join(removed, ", "))
	}
	// Delete the files of removed models unless a remaining model uses them.
	inUse := map[string]bool{}
	for _, m := range merged {
		for _, f := range modelFiles(m) {
			inUse[f] = true
		}
	}
	for _, f := range oldFiles {
		if inUse[f] || strings.Contains(f, "..") || filepath.IsAbs(f) {
			continue
		}
		p := filepath.Join(drive, filepath.FromSlash(f))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if err := os.Remove(p); err == nil {
				fmt.Printf("  deleted %s (%.1f GB freed)\n", f, float64(st.Size())/(1<<30))
			}
		}
	}
	return nil
}

// modelFiles lists the drive-relative files a config.json model entry uses.
func modelFiles(m map[string]any) []string {
	var out []string
	for _, k := range []string{"file", "mmproj"} {
		if f, ok := m[k].(string); ok && f != "" {
			out = append(out, f)
		}
	}
	if xs, ok := m["extra"].([]any); ok {
		for _, x := range xs {
			if xm, ok := x.(map[string]any); ok {
				if f, ok := xm["file"].(string); ok && f != "" {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// ---- Verify ----

// verifyDrive checks every model file on the drive against its expected
// size and SHA-256, catching copies that were cut short or corrupted.
func verifyDrive(drive string) error {
	cfg, err := config.Load(drive)
	if err != nil {
		return err
	}
	bad := 0
	check := func(label, rel string, size int64, sum string) {
		path := cfg.Path(rel)
		st, err := os.Stat(path)
		if err != nil {
			return // not on this drive; nothing to verify
		}
		fmt.Printf("%-40s ", label)
		switch {
		case size > 0 && st.Size() != size:
			fmt.Printf("BAD: %d bytes, expected %d (incomplete copy)\n", st.Size(), size)
			bad++
		case sum == "":
			fmt.Println("ok (size only; no checksum in config)")
		default:
			if err := verify(path, sum); err != nil {
				fmt.Println("BAD: checksum mismatch (file is corrupted)")
				bad++
			} else {
				fmt.Println("ok")
			}
		}
	}
	for _, m := range cfg.Models {
		if m.Base != "" {
			continue // a personality: its files are the base model's
		}
		check(m.ID, m.File, m.Size, m.SHA256)
		for _, x := range m.Extra {
			check(m.ID+" "+filepath.Base(x.File), x.File, x.Size, x.SHA256)
		}
		if m.MMProj != "" {
			check(m.ID+" (projector)", m.MMProj, m.MMProjSize, m.MMProjSHA256)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d file(s) damaged: copy them to the drive again, or run fetch-models -drive %s", bad, drive)
	}
	fmt.Println("All model files are intact.")
	return nil
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
			if err := cfg.Check(m); err != nil {
				mark = "INCOMPLETE: " + err.Error()
			}
		}
		if m.Role == "voice" && len(m.Extra) > 0 {
			if err := cfg.Check(m); err == nil {
				mark += fmt.Sprintf(", speaks (%d files)", len(m.Extra)+1)
			} else if cfg.Present(m) {
				mark = "INCOMPLETE: " + err.Error()
			}
		}
		if m.MMProj != "" && m.Role == "speech" {
			if cfg.Vision(m) {
				mark += ", hears speech"
			} else if cfg.Present(m) {
				mark += ", audio encoder missing"
			}
		} else if m.MMProj != "" {
			if cfg.Vision(m) {
				mark += ", sees images"
			} else if cfg.Present(m) {
				mark += ", image projector missing"
			}
		}
		if m.Base != "" {
			mark += ", personality of " + m.Base
		}
		fmt.Printf("  %-16s %-10s %s\n", m.ID, m.Role, mark)
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

// revision reads a config entry's revision number (0 when absent).
func revision(m map[string]any) int {
	n, _ := m["revision"].(float64)
	return int(n)
}
