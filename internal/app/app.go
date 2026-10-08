// Package app ties the drive together: it picks a runtime and model for the
// host, runs the llama.cpp servers, keeps the encrypted vault, and serves the
// local web UI and API.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/llama"
	"github.com/ninovee/usbai/internal/platform"
	"github.com/ninovee/usbai/internal/rag"
	"github.com/ninovee/usbai/internal/vault"
)

// Engine states reported to the UI.
const (
	StateStarting = "starting"
	StateReady    = "ready"
	StateError    = "error"
	StateNoModel  = "no_model"
	StateOff      = "off"
)

// App is the running assistant.
type App struct {
	// Version is the build version shown in Settings.
	Version string

	cfg     config.Config
	host    platform.Info
	dataDir string
	log     io.Writer

	ctx    context.Context
	cancel context.CancelFunc

	// Engine state.
	engMu       sync.Mutex
	chat        *llama.Server
	chatModel   config.Model
	chatRuntime string
	chatState   string
	chatErr     string
	chatWarn    string    // set when a fallback model is in use
	chatSince   time.Time // when the current chat model started loading
	chatLoadDur time.Duration
	embed       *llama.Server
	embedModel  config.Model
	embedState  string
	engGen      int // bumps on every chat-engine restart
	// chatLoadCancel stops the chat model load in progress.
	chatLoadCancel context.CancelFunc

	// Image reader: a second, vision-capable model that describes images
	// when the chat model can't see them.
	vision      *llama.Server
	visionModel config.Model
	visionState string
	visionErr   string
	visionGen   int

	// Speech recognition (talk-to-text), see speech.go.
	speech      *llama.Server
	speechModel config.Model
	speechState string

	// Unlocked user data; nil vault means locked.
	dataMu sync.RWMutex
	vault  *vault.Vault
	index  *rag.Index
	docs   map[string]DocMeta
	memory []MemoryItem
	// cacheRoot overrides the model cache location (tests); cacheMu
	// serializes cache copies.
	cacheRoot string
	cacheMu   sync.Mutex

	// agentsMu serializes read-modify-write of the agents list.
	agentsMu sync.Mutex

	// Shutdown is closed when the user asks to shut down from the UI.
	Shutdown     chan struct{}
	shutdownOnce sync.Once
	stopOnce     sync.Once
}

// New creates the app. Call StartEngines to load the models.
func New(cfg config.Config, host platform.Info, log io.Writer) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		cfg:         cfg,
		host:        host,
		dataDir:     cfg.Path("data"),
		log:         log,
		ctx:         ctx,
		cancel:      cancel,
		chatState:   StateStarting,
		embedState:  StateStarting,
		speechState: StateStarting,
		visionState: StateOff,
		Shutdown:    make(chan struct{}),
	}
}

func (a *App) logf(format string, args ...any) { fmt.Fprintf(a.log, format+"\n", args...) }

// settings are non-secret preferences kept outside the vault so they apply
// before unlocking.
type settings struct {
	ChatModel string `json:"chat_model,omitempty"`
	// VisionModel picks the image reader: "" automatic, "off", or a model id.
	VisionModel string `json:"vision_model,omitempty"`
	// CacheModels keeps copies of models on the host for faster loading.
	CacheModels bool `json:"cache_models,omitempty"`
}

func (a *App) loadSettings() settings {
	var s settings
	b, err := os.ReadFile(filepath.Join(a.dataDir, "settings.json"))
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (a *App) saveSettings(s settings) error {
	if err := os.MkdirAll(a.dataDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(filepath.Join(a.dataDir, "settings.json"), b, 0o600)
}

// chatCandidates orders the usable chat models to try: the user's saved
// choice, then models in config order that fit in RAM, then the rest from
// smallest. Models whose files are missing or damaged are left out and
// described in problems.
func (a *App) chatCandidates() (cands []config.Model, problems []string) {
	var usable []config.Model
	for _, m := range a.cfg.ModelsByRole("chat") {
		if !a.cfg.Present(m) {
			continue
		}
		if err := a.cfg.Check(m); err != nil {
			problems = append(problems, m.Name+": "+err.Error())
			continue
		}
		usable = append(usable, m)
	}
	seen := map[string]bool{}
	add := func(m config.Model) {
		if !seen[m.ID] {
			seen[m.ID] = true
			cands = append(cands, m)
		}
	}
	if want := a.loadSettings().ChatModel; want != "" {
		for _, m := range usable {
			if m.ID == want {
				add(m)
			}
		}
	}
	ram := a.host.RAMGB()
	for _, m := range usable {
		if ram == 0 || m.MinRAMGB <= ram {
			add(m)
		}
	}
	rest := append([]config.Model(nil), usable...)
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].MinRAMGB < rest[j].MinRAMGB })
	for _, m := range rest {
		add(m)
	}
	return cands, problems
}

// StartEngines loads the chat and embedding models in the background.
func (a *App) StartEngines() {
	// One at a time: models loading in parallel from the same USB drive
	// slow each other down. The chat model comes first; the small
	// embedding model (document search), the image reader and speech
	// recognition follow.
	go func() {
		a.startChat()
		a.startEmbed()
		a.startSpeech()
	}()
}

// threadsFor picks CPU threads for a model's engine. The speech model is
// small and shares the computer with the chat model and the browser, so it
// gets half the cores: llama.cpp threads spin while waiting, and engines
// that each take every core slow each other down badly.
func (a *App) threadsFor(m config.Model) int {
	if a.cfg.Threads > 0 || m.Role != "speech" {
		return a.cfg.Threads
	}
	return max(2, runtime.NumCPU()/2)
}

// startServer tries each runtime candidate (GPU builds first, CPU last) and
// returns the first that loads the model.
func (a *App) startServer(ctx context.Context, m config.Model, embedding bool) (*llama.Server, string, error) {
	// Load from the host's cache when possible; queue copies otherwise.
	var toCache []cachedFile
	mf := cachedFile{rel: m.File, size: m.Size, sum: m.SHA256}
	modelPath, hit := a.loadPath(mf)
	if !hit {
		toCache = append(toCache, mf)
	}
	mmproj := ""
	if !embedding && a.cfg.Vision(m) {
		pf := cachedFile{rel: m.MMProj, size: m.MMProjSize, sum: m.MMProjSHA256}
		var phit bool
		if mmproj, phit = a.loadPath(pf); !phit {
			toCache = append(toCache, pf)
		}
	}
	if modelPath != a.cfg.Path(m.File) {
		a.logf("Loading %s from this computer's cache: %s", m.Name, modelPath)
	}
	var errs []error
	tried := false
	for _, name := range a.host.RuntimeCandidates() {
		dir := a.cfg.Path(filepath.Join("runtime", name))
		if _, err := os.Stat(filepath.Join(dir, a.host.ServerBinary())); err != nil {
			continue
		}
		tried = true
		gpu := a.cfg.GPULayers
		if strings.HasSuffix(name, "-cpu") {
			gpu = 0
		}
		prefix := "chat"
		if embedding {
			prefix = "embed"
		} else if m.Role == "speech" {
			prefix = "speech"
		}
		a.logf("Starting %s model %q with runtime %s", prefix, m.Name, name)
		srv, err := llama.Start(ctx, llama.Options{
			RuntimeDir: dir,
			Binary:     a.host.ServerBinary(),
			Model:      modelPath,
			Context:    m.Context,
			GPULayers:  gpu,
			Threads:    a.threadsFor(m),
			Embedding:  embedding,
			MMProj:     mmproj,
			ExtraArgs:  a.cfg.ExtraArgs,
			LogPrefix:  prefix + "/" + name,
			Log:        a.log,
		})
		if err == nil {
			if len(toCache) > 0 {
				go a.cacheFiles(toCache)
			}
			return srv, name, nil
		}
		a.logf("Runtime %s failed: %v", name, err)
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
		if ctx.Err() != nil {
			break
		}
	}
	if !tried {
		return nil, "", fmt.Errorf("no runtime for %s on this drive: looked for %s in %s/{%s}; %s",
			a.host.Slug(), a.host.ServerBinary(), a.cfg.Path("runtime"),
			strings.Join(a.host.RuntimeCandidates(), ","), describeDir(a.cfg.Path("runtime")))
	}
	return nil, "", errors.Join(errs...)
}

// describeDir summarizes what is in dir, to make "file not found" errors
// diagnosable from a screenshot.
func describeDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "that folder cannot be read: " + err.Error()
	}
	var parts []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n := e.Name()
		if e.IsDir() {
			if sub, err := os.ReadDir(filepath.Join(dir, n)); err == nil {
				n = fmt.Sprintf("%s/ (%d files)", n, len(sub))
			}
		}
		parts = append(parts, n)
	}
	if len(parts) == 0 {
		return "that folder is empty — run drivetool fetch-runtime and copy runtime/ to the drive"
	}
	return "found: " + strings.Join(parts, ", ")
}

func (a *App) startChat() {
	a.engMu.Lock()
	a.engGen++
	gen := a.engGen
	// Only one chat model loads at a time: two big models loading at
	// once can need more memory than the computer has.
	if a.chatLoadCancel != nil {
		a.chatLoadCancel()
	}
	loadCtx, cancel := context.WithCancel(a.ctx)
	a.chatLoadCancel = cancel
	old := a.chat
	a.chat = nil
	a.chatState, a.chatErr, a.chatWarn = StateStarting, "", ""
	cands, problems := a.chatCandidates()
	if len(cands) == 0 {
		a.chatState = StateNoModel
		a.chatErr = "no usable chat model in models/ — run drivetool fetch-models"
		if len(problems) > 0 {
			a.chatErr = strings.Join(problems, "\n")
		}
		a.engMu.Unlock()
		old.Stop()
		return
	}
	a.chatModel = cands[0]
	a.chatSince = time.Now()
	a.engMu.Unlock()
	old.Stop()

	// Try each candidate until one loads, so one damaged or oversized model
	// never leaves the user without an assistant.
	var srv *llama.Server
	var rt string
	for i, m := range cands {
		if i > 0 {
			a.engMu.Lock()
			if gen != a.engGen {
				a.engMu.Unlock()
				return
			}
			a.chatModel = m
			a.engMu.Unlock()
		}
		var err error
		srv, rt, err = a.startServer(loadCtx, m, false)
		if err == nil {
			break
		}
		problems = append(problems, m.Name+": "+firstLine(err))
		if loadCtx.Err() != nil {
			return // shutting down, or a newer load took over
		}
	}

	a.engMu.Lock()
	defer a.engMu.Unlock()
	if gen != a.engGen || a.ctx.Err() != nil {
		srv.Stop() // superseded by a newer start or shut down
		return
	}
	if srv == nil {
		a.chatState, a.chatErr = StateError, strings.Join(problems, "\n")
		return
	}
	a.chat, a.chatRuntime, a.chatState = srv, rt, StateReady
	a.chatLoadDur = time.Since(a.chatSince).Round(time.Second)
	if len(problems) > 0 {
		a.chatWarn = "Using " + a.chatModel.Name + " because:\n" + strings.Join(problems, "\n")
	}
	a.logf("Chat model ready: %s (%s), loaded in %s", a.chatModel.Name, rt, a.chatLoadDur)
	go a.startVision()
}

// visionHelperMinRAMGB is the memory below which an image reader is not
// started automatically alongside the chat model.
const visionHelperMinRAMGB = 16

// pickVisionHelper chooses the image-reader model, or explains why none.
// memHeadroom is the memory left for the system, the browser and the
// small engines when deciding whether two models fit.
const memHeadroom = 4 << 30

func (a *App) pickVisionHelper(chat config.Model) (config.Model, string) {
	if a.cfg.Vision(chat) {
		return config.Model{}, "not needed: " + chat.Name + " can see images itself"
	}
	want := a.loadSettings().VisionModel
	if want == "off" {
		return config.Model{}, "turned off in Settings"
	}
	var usable []config.Model
	for _, m := range a.cfg.ModelsByRole("chat") {
		if m.ID != chat.ID && a.cfg.Vision(m) && a.cfg.Check(m) == nil {
			usable = append(usable, m)
		}
	}
	if want != "" {
		for _, m := range usable {
			if m.ID == want {
				return m, ""
			}
		}
		return config.Model{}, "the chosen image reader is not on this drive"
	}
	if len(usable) == 0 {
		return config.Model{}, "no vision model on this drive"
	}
	if ram := a.host.RAMGB(); ram > 0 && ram < visionHelperMinRAMGB {
		return config.Model{}, fmt.Sprintf("this computer has %d GB of memory; choose an image reader in Settings to run one anyway", ram)
	}
	// The smallest vision model: it only has to read images.
	sort.SliceStable(usable, func(i, j int) bool { return usable[i].Size+usable[i].MMProjSize < usable[j].Size+usable[j].MMProjSize })
	// Both models must fit in memory next to the system and the browser;
	// otherwise the system kills one of them.
	if need := chat.Size + usable[0].Size + usable[0].MMProjSize + memHeadroom; a.host.RAMBytes > 0 && chat.Size > 0 && need > int64(a.host.RAMBytes) {
		return config.Model{}, fmt.Sprintf("%s and an image reader need about %d GB of memory together; this computer has %d GB. Choose an image reader in Settings to run one anyway",
			chat.Name, (need+(1<<30)-1)>>30, a.host.RAMGB())
	}
	return usable[0], ""
}

// startVision starts, keeps or stops the image reader to match the current
// chat model and settings.
func (a *App) startVision() {
	a.engMu.Lock()
	chat := a.chatModel
	m, why := a.pickVisionHelper(chat)
	if a.vision != nil && a.vision.Alive() && why == "" && a.visionModel.ID == m.ID {
		a.engMu.Unlock()
		return // already running the right model
	}
	a.visionGen++
	gen := a.visionGen
	old := a.vision
	a.vision = nil
	if why != "" {
		a.visionModel, a.visionState, a.visionErr = config.Model{}, StateOff, why
		a.engMu.Unlock()
		old.Stop()
		return
	}
	a.visionModel, a.visionState, a.visionErr = m, StateStarting, ""
	a.engMu.Unlock()
	old.Stop()

	m.Context = 4096 // enough to describe an image
	srv, _, err := a.startServer(a.ctx, m, false)

	a.engMu.Lock()
	defer a.engMu.Unlock()
	if gen != a.visionGen || a.ctx.Err() != nil {
		srv.Stop()
		return
	}
	if err != nil {
		a.visionState, a.visionErr = StateError, m.Name+": "+firstLine(err)
		return
	}
	a.vision, a.visionState = srv, StateReady
	a.logf("Image reader ready: %s", m.Name)
}

// visionEngine returns the image reader if it is running.
func (a *App) visionEngine() (string, config.Model, bool) {
	a.engMu.Lock()
	defer a.engMu.Unlock()
	if a.vision == nil || !a.vision.Alive() {
		return "", config.Model{}, false
	}
	return a.vision.BaseURL, a.visionModel, true
}

// firstLine keeps the informative part of a llama-server failure: the
// "failed to load" lines, else the first line.
func firstLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	for _, l := range lines {
		if strings.Contains(l, "error loading model") || strings.Contains(l, "not within the file bounds") || strings.Contains(l, "out of memory") {
			if i := strings.Index(l, " E "); i >= 0 {
				l = l[i+3:]
			}
			return strings.TrimSpace(l)
		}
	}
	return lines[0]
}

func (a *App) startEmbed() {
	models := a.cfg.ModelsByRole("embedding")
	var m *config.Model
	for i := range models {
		if a.cfg.Check(models[i]) == nil {
			m = &models[i]
			break
		}
	}
	a.engMu.Lock()
	if m == nil {
		a.embedState = StateNoModel
		a.engMu.Unlock()
		a.logf("No embedding model found; document search will use keywords only")
		return
	}
	a.embedModel = *m
	a.engMu.Unlock()

	srv, _, err := a.startServer(a.ctx, *m, true)

	a.engMu.Lock()
	if a.ctx.Err() != nil {
		a.engMu.Unlock()
		srv.Stop()
		return
	}
	if err != nil {
		a.embedState = StateError
		a.engMu.Unlock()
		a.logf("Embedding model failed; document search will use keywords only: %v", err)
		return
	}
	a.embed, a.embedState = srv, StateReady
	a.engMu.Unlock()
	a.logf("Embedding model ready: %s", m.Name)
	go a.backfillEmbeddings()
}

// chatEngine returns the chat server URL and model if ready.
func (a *App) chatEngine() (string, config.Model, bool) {
	a.engMu.Lock()
	defer a.engMu.Unlock()
	if a.chat == nil || !a.chat.Alive() {
		return "", config.Model{}, false
	}
	return a.chat.BaseURL, a.chatModel, true
}

// embedEngine returns the embedding server URL and model if ready.
func (a *App) embedEngine() (string, config.Model, bool) {
	a.engMu.Lock()
	defer a.engMu.Unlock()
	if a.embed == nil || !a.embed.Alive() {
		return "", config.Model{}, false
	}
	return a.embed.BaseURL, a.embedModel, true
}

// Stop shuts down the model servers and forgets the vault key.
func (a *App) Stop() {
	a.stopOnce.Do(func() {
		a.cancel()
		a.lock()
		a.engMu.Lock()
		chat, embed, vision, speech := a.chat, a.embed, a.vision, a.speech
		a.chat, a.embed, a.vision, a.speech = nil, nil, nil, nil
		a.engMu.Unlock()
		chat.Stop()
		embed.Stop()
		vision.Stop()
		speech.Stop()
	})
}
