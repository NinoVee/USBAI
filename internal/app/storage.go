package app

import (
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ninovee/usbai/internal/platform"
)

// StorageReport says what is using the drive, for the bar in the menu.
// Your data is measured from the encrypted vault: its object names are
// only known while it is unlocked, so the split into files, chats, memory
// and agents needs an unlocked vault.
type StorageReport struct {
	Total   int64            `json:"total"`
	Free    int64            `json:"free"`
	Parts   map[string]int64 `json:"parts"` // models, app, files, chats, memory, agents, data, other
	Locked  bool             `json:"locked"`
	Cache   int64            `json:"cache"` // model copies on this computer (not the drive)
	Checked time.Time        `json:"checked"`
}

var storageCache struct {
	sync.Mutex
	report StorageReport
	locked bool
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// vaultCategory sorts vault object names into the storage bar's parts.
func vaultCategory(name string) string {
	switch {
	case strings.HasPrefix(name, "docs/"):
		return "files"
	case strings.HasPrefix(name, "chats/"), strings.HasPrefix(name, "images/"):
		return "chats"
	case name == "memory":
		return "memory"
	case name == "agents":
		return "agents"
	}
	return "data" // settings and the vault's own index
}

// Storage measures the drive; results are reused for 15 seconds.
func (a *App) Storage() (StorageReport, error) {
	v, lockErr := a.unlocked()
	locked := lockErr != nil
	storageCache.Lock()
	defer storageCache.Unlock()
	if r := storageCache.report; !r.Checked.IsZero() && time.Since(r.Checked) < 15*time.Second && storageCache.locked == locked {
		return r, nil
	}
	total, free, err := platform.DiskSpace(a.cfg.Root)
	if err != nil {
		return StorageReport{}, err
	}
	r := StorageReport{Total: total, Free: free, Parts: map[string]int64{}, Locked: locked, Checked: time.Now()}
	r.Parts["models"] = dirSize(a.cfg.Path("models"))
	r.Parts["app"] = dirSize(a.cfg.Path("runtime")) + dirSize(a.cfg.Path("bin"))
	if locked {
		r.Parts["data"] = dirSize(a.vaultDir())
	} else {
		for k, n := range v.Usage(vaultCategory) {
			if k == "" {
				k = "data"
			}
			r.Parts[k] += n
		}
	}
	var known int64
	for _, n := range r.Parts {
		known += n
	}
	// Everything else on the drive: other files, settings, logs, and the
	// file system's own overhead.
	if other := total - free - known; other > 0 {
		r.Parts["other"] = other
	}
	_, r.Cache = a.CacheStatus()
	storageCache.report, storageCache.locked = r, locked
	return r, nil
}

// forgetStorage makes the next report measure again (after the cache is
// cleared, for example).
func forgetStorage() {
	storageCache.Lock()
	storageCache.report = StorageReport{}
	storageCache.Unlock()
}
