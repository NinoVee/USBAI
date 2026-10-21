package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/fetch"
	"github.com/ninovee/usbai/internal/platform"
)

// Model manager: download models listed in config.json, delete models from
// the drive, and add new GGUF models from Hugging Face. Changes are written
// to the drive's config.json, so they last. The internet is only used when
// the user downloads or searches.

// hfBase is Hugging Face; a variable so tests can use a local server.
var hfBase = "https://huggingface.co"

// Download is one model download in progress (or just finished).
type Download struct {
	State string `json:"state"` // "downloading", "done" or "error"
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Error string `json:"error,omitempty"`

	cancel context.CancelFunc
}

type downloads struct {
	mu sync.Mutex
	m  map[string]*Download
}

// Downloads reports the downloads, by model id.
func (a *App) Downloads() map[string]Download {
	a.dls.mu.Lock()
	defer a.dls.mu.Unlock()
	out := map[string]Download{}
	for id, d := range a.dls.m {
		out[id] = *d
	}
	return out
}

func (a *App) findModel(id string) (config.Model, bool) {
	for _, m := range a.models() {
		if m.ID == id {
			return m, true
		}
	}
	return config.Model{}, false
}

// modelFile is one file of a model to download.
type modelFile struct {
	rel, url, sha string
	size          int64
}

func modelFiles(m config.Model) []modelFile {
	files := []modelFile{{m.File, m.URL, m.SHA256, m.Size}}
	if m.MMProj != "" {
		files = append(files, modelFile{m.MMProj, m.MMProjURL, m.MMProjSHA256, m.MMProjSize})
	}
	for _, x := range m.Extra {
		if x.Member == "" { // files inside archives are drivetool's job
			files = append(files, modelFile{x.File, x.URL, x.SHA256, x.Size})
		}
	}
	return files
}

// DownloadModel downloads a model listed in config.json to the drive, in
// the background.
func (a *App) DownloadModel(id string) error {
	if _, err := a.unlocked(); err != nil {
		return err
	}
	m, ok := a.findModel(id)
	if !ok {
		return fmt.Errorf("no model %q", id)
	}
	if m.Cloud() || m.Base != "" {
		return errors.New(m.Name + " has no files to download")
	}
	var need, total int64
	var todo []modelFile
	for _, f := range modelFiles(m) {
		if f.url == "" {
			return errors.New(m.Name + " has no download address in config.json")
		}
		total += f.size
		if a.cfg.CheckFile(f.rel, f.size) != nil {
			todo = append(todo, f)
			need += f.size
		}
	}
	if len(todo) == 0 {
		return errors.New(m.Name + " is already on the drive")
	}
	if _, free, err := platform.DiskSpace(a.cfg.Root); err == nil && need > 0 && free < need+(1<<30) {
		return fmt.Errorf("not enough space on the drive: %s needs %s and %s is free — delete a model first",
			m.Name, humanSizeGB(need), humanSizeGB(free))
	}
	a.dls.mu.Lock()
	if d := a.dls.m[id]; d != nil && d.State == "downloading" {
		a.dls.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(a.ctx)
	d := &Download{State: "downloading", Total: total, cancel: cancel}
	a.dls.m[id] = d
	a.dls.mu.Unlock()
	go a.runDownload(ctx, m, todo, d)
	return nil
}

func (a *App) runDownload(ctx context.Context, m config.Model, todo []modelFile, d *Download) {
	var err error
	var already int64 = d.Total
	for _, f := range todo {
		already -= f.size
	}
	for _, f := range todo {
		a.logf("Downloading %s: %s", m.Name, f.url)
		base := already
		err = fetch.Download(ctx, f.url, a.cfg.Path(f.rel), f.sha, func(done, total int64) {
			a.dls.mu.Lock()
			d.Done = base + done
			a.dls.mu.Unlock()
		})
		if err != nil {
			break
		}
		already += f.size
	}
	a.dls.mu.Lock()
	switch {
	case errors.Is(err, context.Canceled):
		delete(a.dls.m, m.ID)
	case err != nil:
		d.State, d.Error = "error", err.Error()
	default:
		d.State, d.Done = "done", d.Total
	}
	a.dls.mu.Unlock()
	forgetStorage()
	if err != nil {
		a.logf("Download of %s stopped: %v", m.Name, err)
		return
	}
	a.logf("Downloaded %s", m.Name)
	// Start the engine that was waiting for a model like this one.
	a.engMu.Lock()
	chatIdle := a.chatState == StateNoModel || a.chatState == StateError
	embedIdle := a.embedState == StateNoModel
	a.engMu.Unlock()
	switch {
	case m.Role == "chat" && chatIdle:
		go a.startChat()
	case m.Role == "embedding" && embedIdle:
		go a.startEmbed()
	}
}

// CancelDownload stops a download; its .part file is kept, so starting it
// again resumes.
func (a *App) CancelDownload(id string) {
	a.dls.mu.Lock()
	defer a.dls.mu.Unlock()
	if d := a.dls.m[id]; d != nil {
		if d.cancel != nil {
			d.cancel()
		}
		if d.State != "downloading" {
			delete(a.dls.m, id)
		}
	}
}

// DeleteModel removes a model's files from the drive. Models added from
// the app lose their entry too; models from the drive's list keep it, so
// they can be downloaded again. force must be set to delete a model that
// personalities (such as GANG) are built on.
func (a *App) DeleteModel(id string, force bool) (freed int64, err error) {
	if _, err := a.unlocked(); err != nil {
		return 0, err
	}
	m, ok := a.findModel(id)
	if !ok {
		return 0, fmt.Errorf("no model %q", id)
	}
	if m.Cloud() || m.Base != "" {
		return 0, errors.New(m.Name + " has no files on the drive")
	}
	a.engMu.Lock()
	busy := (a.chatModel.ID == id || a.chatModel.Base == id) && (a.chatState == StateReady || a.chatState == StateStarting)
	busy = busy || (a.visionModel.ID == id && a.visionState != StateOff) || (a.embedModel.ID == id && a.embedState == StateReady) ||
		(a.speechModel.ID == id && a.speechState == StateReady)
	a.engMu.Unlock()
	if busy {
		return 0, errors.New(m.Name + " is in use — switch to another model in Settings first")
	}
	if users := a.personalitiesOf(id); len(users) > 0 && !force {
		return 0, fmt.Errorf("%s is used by %s; deleting it removes them too", m.Name, strings.Join(users, ", "))
	}
	a.CancelDownload(id)
	// Files another model also uses stay.
	shared := map[string]bool{}
	for _, o := range a.models() {
		if o.ID != id && o.Base == "" && !o.Cloud() {
			for _, f := range modelFiles(o) {
				shared[f.rel] = true
			}
		}
	}
	for _, f := range modelFiles(m) {
		if shared[f.rel] {
			continue
		}
		p := a.cfg.Path(f.rel)
		if st, err := os.Stat(p); err == nil {
			freed += st.Size()
		}
		for _, q := range []string{p, p + ".part"} {
			if err := os.Remove(q); err != nil && !os.IsNotExist(err) {
				return freed, err
			}
		}
	}
	if m.Added != "" {
		if dir := filepath.Dir(a.cfg.Path(m.File)); strings.Contains(filepath.ToSlash(dir), "/models/chat/hf/") {
			os.Remove(dir) // the repo's folder, if now empty
		}
		err = a.updateModels(func(ms []config.Model, raw []map[string]any) ([]config.Model, []map[string]any) {
			return without(ms, raw, id)
		})
	}
	a.dls.mu.Lock()
	delete(a.dls.m, id)
	a.dls.mu.Unlock()
	forgetStorage()
	a.logf("Deleted %s (%s freed)", m.Name, humanSizeGB(freed))
	return freed, err
}

// personalitiesOf names the personalities built on a model.
func (a *App) personalitiesOf(id string) []string {
	var out []string
	for _, o := range a.models() {
		if o.Base == id {
			out = append(out, o.Name)
		}
	}
	return out
}

func without(ms []config.Model, raw []map[string]any, id string) ([]config.Model, []map[string]any) {
	var outM []config.Model
	for _, m := range ms {
		if m.ID != id {
			outM = append(outM, m)
		}
	}
	var outR []map[string]any
	for _, r := range raw {
		if fmt.Sprint(r["id"]) != id {
			outR = append(outR, r)
		}
	}
	return outM, outR
}

// updateModels changes the model list in memory and in the drive's
// config.json together, keeping every other setting in the file as it is.
func (a *App) updateModels(change func([]config.Model, []map[string]any) ([]config.Model, []map[string]any)) error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	p := filepath.Join(a.cfg.Root, config.FileName)
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(b, &file); err != nil {
		return fmt.Errorf("%s: %w", config.FileName, err)
	}
	var raw []map[string]any
	json.Unmarshal(file["models"], &raw)
	models, raw := change(append([]config.Model(nil), a.cfg.Models...), raw)
	enc, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	file["models"] = enc
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	a.cfg.Models = models
	return nil
}

// ---- Hugging Face ----

// HFModel is one search result.
type HFModel struct {
	ID        string `json:"id"`
	Downloads int    `json:"downloads"`
	Likes     int    `json:"likes"`
	Gated     bool   `json:"gated"`
}

// HFFile is one GGUF file in a repository.
type HFFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
	// FitsRAM is false when the file looks too big for this computer.
	FitsRAM bool `json:"fits_ram"`
}

// HFFiles lists a repository's model files and image projectors.
type HFFiles struct {
	Repo   string   `json:"repo"`
	Models []HFFile `json:"models"`
	MMProj []HFFile `json:"mmproj"`
}

var reHFRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

func hfGet(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hfBase+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Hugging Face: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("Hugging Face has no such model")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errors.New("this model needs a Hugging Face login or an accepted license, which GANG AI doesn't support")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("Hugging Face: %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// SearchHF finds GGUF models on Hugging Face, most downloaded first.
func (a *App) SearchHF(ctx context.Context, query string) ([]HFModel, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("type what to search for, e.g. qwen3 8b")
	}
	q := url.Values{"search": {query}, "filter": {"gguf"}, "sort": {"downloads"}, "direction": {"-1"}, "limit": {"25"}}
	var raw []struct {
		ID        string `json:"id"`
		Downloads int    `json:"downloads"`
		Likes     int    `json:"likes"`
		Gated     any    `json:"gated"`
	}
	if err := hfGet(ctx, "/api/models?"+q.Encode(), &raw); err != nil {
		return nil, err
	}
	out := []HFModel{}
	for _, r := range raw {
		gated := r.Gated != nil && r.Gated != false
		out = append(out, HFModel{ID: r.ID, Downloads: r.Downloads, Likes: r.Likes, Gated: gated})
	}
	return out, nil
}

// reSplitGGUF matches the later parts of a model split into several files.
var reSplitGGUF = regexp.MustCompile(`(?i)-\d{5}-of-\d{5}\.gguf$`)

// HFRepoFiles lists a repository's single-file GGUF models and image
// projectors.
func (a *App) HFRepoFiles(ctx context.Context, repo string) (HFFiles, error) {
	if !reHFRepo.MatchString(repo) {
		return HFFiles{}, errors.New("not a Hugging Face model name (owner/name)")
	}
	var tree []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := hfGet(ctx, "/api/models/"+repo+"/tree/main?recursive=true", &tree); err != nil {
		return HFFiles{}, err
	}
	ram := int64(a.host.RAMBytes)
	out := HFFiles{Repo: repo, Models: []HFFile{}, MMProj: []HFFile{}}
	for _, f := range tree {
		if f.Type != "file" || !strings.HasSuffix(strings.ToLower(f.Path), ".gguf") || reSplitGGUF.MatchString(f.Path) {
			continue
		}
		hf := HFFile{Path: f.Path, Size: f.Size}
		if f.LFS != nil {
			hf.SHA256, hf.Size = f.LFS.OID, f.LFS.Size
		}
		hf.FitsRAM = ram == 0 || hf.Size+memHeadroom <= ram
		if strings.Contains(strings.ToLower(path.Base(f.Path)), "mmproj") {
			out.MMProj = append(out.MMProj, hf)
		} else {
			out.Models = append(out.Models, hf)
		}
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Size < out.Models[j].Size })
	sort.Slice(out.MMProj, func(i, j int) bool { return out.MMProj[i].Size < out.MMProj[j].Size })
	return out, nil
}

var reIDChars = regexp.MustCompile(`[^a-z0-9]+`)

// AddHFModel adds a GGUF model from Hugging Face to the drive's list and
// starts downloading it. The file names and checksums come from Hugging
// Face, not from the request.
func (a *App) AddHFModel(ctx context.Context, repo, file, mmproj string) (config.Model, error) {
	if _, err := a.unlocked(); err != nil {
		return config.Model{}, err
	}
	files, err := a.HFRepoFiles(ctx, repo)
	if err != nil {
		return config.Model{}, err
	}
	find := func(list []HFFile, p string) (HFFile, bool) {
		for _, f := range list {
			if f.Path == p {
				return f, true
			}
		}
		return HFFile{}, false
	}
	f, ok := find(files.Models, file)
	if !ok {
		return config.Model{}, fmt.Errorf("%s has no file %s", repo, file)
	}
	dir := "models/chat/hf/" + strings.ReplaceAll(repo, "/", "__") + "/"
	resolve := func(p string) string {
		segs := strings.Split(p, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		return hfBase + "/" + repo + "/resolve/main/" + strings.Join(segs, "/")
	}
	quant := strings.TrimSuffix(path.Base(file), path.Ext(file))
	m := config.Model{
		ID:      "hf-" + strings.Trim(reIDChars.ReplaceAllString(strings.ToLower(path.Base(repo)+"-"+quant), "-"), "-"),
		Name:    path.Base(repo) + " · " + quant + " (Hugging Face)",
		Role:    "chat",
		File:    dir + path.Base(file),
		URL:     resolve(file),
		Size:    f.Size,
		SHA256:  f.SHA256,
		Context: 8192,
		Added:   "huggingface",
		HFRepo:  repo,
	}
	total := f.Size
	if mmproj != "" {
		pf, ok := find(files.MMProj, mmproj)
		if !ok {
			return config.Model{}, fmt.Errorf("%s has no image projector %s", repo, mmproj)
		}
		m.MMProj, m.MMProjURL, m.MMProjSize, m.MMProjSHA256 = dir+path.Base(mmproj), resolve(mmproj), pf.Size, pf.SHA256
		total += pf.Size
	}
	// The model, its working memory and the rest of the system.
	m.MinRAMGB = int(math.Ceil(float64(total)*1.2/(1<<30))) + 2
	if _, exists := a.findModel(m.ID); exists {
		return config.Model{}, errors.New(m.Name + " is already in your list")
	}
	raw := map[string]any{}
	b, _ := json.Marshal(m)
	json.Unmarshal(b, &raw)
	if err := a.updateModels(func(ms []config.Model, rs []map[string]any) ([]config.Model, []map[string]any) {
		return append(ms, m), append(rs, raw)
	}); err != nil {
		return config.Model{}, err
	}
	if err := a.DownloadModel(m.ID); err != nil {
		// Don't keep an entry the user can't use yet.
		a.updateModels(func(ms []config.Model, rs []map[string]any) ([]config.Model, []map[string]any) {
			return without(ms, rs, m.ID)
		})
		return config.Model{}, err
	}
	return m, nil
}
