package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/platform"
)

// Model caching (opt-in): USB drives are slow to read, so after a model has
// loaded once, a copy is kept on the host's internal disk and later loads
// read from there. Only public model files are cached — never the vault —
// and copies are named by checksum so file names reveal nothing.

const cacheLimit = 40 << 30 // keep the cache under this size

// cacheHeadroom is always left free on the host disk (a var for tests).
var cacheHeadroom int64 = 10 << 30

// cacheDir is where cached models live, e.g. ~/Library/Caches/PrivateAI/models.
func (a *App) cacheDir() string {
	if a.cacheRoot != "" {
		return a.cacheRoot
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "PrivateAI", "models")
}

func (a *App) cachingOn() bool { return a.loadSettings().CacheModels && a.cacheDir() != "" }

func cacheKey(rel string, size int64, sum string) string {
	if sum == "" {
		h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", rel, size)))
		sum = hex.EncodeToString(h[:])
	}
	return strings.ToLower(sum) + ".gguf"
}

// cachedFile describes one model file the engine will load.
type cachedFile struct {
	rel, sum string
	size     int64
}

// loadPath returns where to load rel from: the cached copy if caching is on
// and a complete copy exists, else the drive. hit is false when a copy
// should be made after loading.
func (a *App) loadPath(f cachedFile) (path string, hit bool) {
	src := a.cfg.Path(f.rel)
	if !a.cachingOn() || f.size <= 0 {
		return src, true
	}
	p := filepath.Join(a.cacheDir(), cacheKey(f.rel, f.size, f.sum))
	if st, err := os.Stat(p); err == nil && st.Size() == f.size {
		now := time.Now()
		os.Chtimes(p, now, now) // most recently used, for eviction
		return p, true
	}
	return src, false
}

// cacheFiles copies files to the cache in the background, one at a time.
func (a *App) cacheFiles(files []cachedFile) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	for _, f := range files {
		if err := a.cacheOne(f); err != nil {
			a.logf("Not caching %s: %v", filepath.Base(f.rel), err)
		}
	}
}

func (a *App) cacheOne(f cachedFile) error {
	dir := a.cacheDir()
	dst := filepath.Join(dir, cacheKey(f.rel, f.size, f.sum))
	if st, err := os.Stat(dst); err == nil && st.Size() == f.size {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	a.evictCache(f.size)
	if free, err := platform.FreeBytes(dir); err == nil && free < f.size+cacheHeadroom {
		return fmt.Errorf("only %.1f GB free on this computer", float64(free)/(1<<30))
	}
	in, err := os.Open(a.cfg.Path(f.rel))
	if err != nil {
		return err
	}
	defer in.Close()
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	a.logf("Caching %s on this computer for faster loading…", filepath.Base(f.rel))
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), ctxReader{a, in})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != f.size {
		err = fmt.Errorf("copied %d of %d bytes", n, f.size)
	}
	if err == nil && f.sum != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), f.sum) {
		err = errors.New("checksum mismatch while copying")
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	a.logf("Cached %s", filepath.Base(f.rel))
	return nil
}

// ctxReader stops a copy when the app shuts down (e.g. drive being ejected).
type ctxReader struct {
	a *App
	r io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.a.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

type cacheEntry struct {
	path string
	size int64
	used time.Time
}

func (a *App) cacheEntries() []cacheEntry {
	dir := a.cacheDir()
	if dir == "" {
		return nil
	}
	ents, _ := os.ReadDir(dir)
	var out []cacheEntry
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gguf") {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, cacheEntry{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		}
	}
	return out
}

// evictCache removes least recently used copies until incoming fits.
func (a *App) evictCache(incoming int64) {
	ents := a.cacheEntries()
	sort.Slice(ents, func(i, j int) bool { return ents[i].used.Before(ents[j].used) })
	var total int64
	for _, e := range ents {
		total += e.size
	}
	for _, e := range ents {
		if total+incoming <= cacheLimit {
			break
		}
		os.Remove(e.path)
		total -= e.size
	}
}

// CacheStatus reports what is cached, by model name.
func (a *App) CacheStatus() (names []string, bytes int64) {
	byKey := map[string]string{}
	for _, m := range a.cfg.Models {
		if m.Base != "" {
			continue
		}
		byKey[cacheKey(m.File, m.Size, m.SHA256)] = m.Name
		if m.MMProj != "" {
			byKey[cacheKey(m.MMProj, m.MMProjSize, m.MMProjSHA256)] = m.Name + " (image projector)"
		}
	}
	for _, e := range a.cacheEntries() {
		bytes += e.size
		if n, ok := byKey[filepath.Base(e.path)]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, bytes
}

// ClearCache deletes every cached model (and unfinished copies).
func (a *App) ClearCache() error {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	dir := a.cacheDir()
	if dir == "" {
		return nil
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if n := e.Name(); strings.HasSuffix(n, ".gguf") || strings.HasSuffix(n, ".part") {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

// cacheRunning caches the models that are loaded right now, so turning
// caching on helps from the next start without reloading anything.
func (a *App) cacheRunning() {
	a.engMu.Lock()
	var files []cachedFile
	add := func(m config.Model, running bool) {
		if !running || m.ID == "" {
			return
		}
		files = append(files, cachedFile{rel: m.File, size: m.Size, sum: m.SHA256})
		if m.MMProj != "" && a.cfg.Vision(m) && m.Role == "chat" {
			files = append(files, cachedFile{rel: m.MMProj, size: m.MMProjSize, sum: m.MMProjSHA256})
		}
	}
	add(a.chatModel, a.chat != nil)
	add(a.visionModel, a.vision != nil)
	add(a.embedModel, a.embed != nil)
	a.engMu.Unlock()
	a.cacheFiles(files)
}
