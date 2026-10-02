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
	"strings"
	"sync"

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
)

// App is the running assistant.
type App struct {
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
	embed       *llama.Server
	embedModel  config.Model
	embedState  string
	engGen      int // bumps on every chat-engine restart

	// Unlocked user data; nil vault means locked.
	dataMu sync.RWMutex
	vault  *vault.Vault
	index  *rag.Index
	docs   map[string]DocMeta
	memory []MemoryItem

	// Shutdown is closed when the user asks to shut down from the UI.
	Shutdown     chan struct{}
	shutdownOnce sync.Once
	stopOnce     sync.Once
}

// New creates the app. Call StartEngines to load the models.
func New(cfg config.Config, host platform.Info, log io.Writer) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{
		cfg:        cfg,
		host:       host,
		dataDir:    cfg.Path("data"),
		log:        log,
		ctx:        ctx,
		cancel:     cancel,
		chatState:  StateStarting,
		embedState: StateStarting,
		Shutdown:   make(chan struct{}),
	}
}

func (a *App) logf(format string, args ...any) { fmt.Fprintf(a.log, format+"\n", args...) }

// settings are non-secret preferences kept outside the vault so they apply
// before unlocking.
type settings struct {
	ChatModel string `json:"chat_model,omitempty"`
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

// pickChatModel chooses the user's saved model if present, otherwise the
// first model in config order that is on the drive and fits in RAM, otherwise
// the smallest model present.
func (a *App) pickChatModel() (config.Model, error) {
	var present []config.Model
	for _, m := range a.cfg.ModelsByRole("chat") {
		if a.cfg.Present(m) {
			present = append(present, m)
		}
	}
	if len(present) == 0 {
		return config.Model{}, errors.New("no chat model found in models/ — run drivetool fetch-models")
	}
	if want := a.loadSettings().ChatModel; want != "" {
		for _, m := range present {
			if m.ID == want {
				return m, nil
			}
		}
	}
	ram := a.host.RAMGB()
	for _, m := range present {
		if ram == 0 || m.MinRAMGB <= ram {
			return m, nil
		}
	}
	smallest := present[0]
	for _, m := range present[1:] {
		if m.MinRAMGB < smallest.MinRAMGB {
			smallest = m
		}
	}
	return smallest, nil
}

// StartEngines loads the chat and embedding models in the background.
func (a *App) StartEngines() {
	go a.startChat()
	go a.startEmbed()
}

// startServer tries each runtime candidate (GPU builds first, CPU last) and
// returns the first that loads the model.
func (a *App) startServer(m config.Model, embedding bool) (*llama.Server, string, error) {
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
		}
		a.logf("Starting %s model %q with runtime %s", prefix, m.Name, name)
		srv, err := llama.Start(a.ctx, llama.Options{
			RuntimeDir: dir,
			Binary:     a.host.ServerBinary(),
			Model:      a.cfg.Path(m.File),
			Context:    m.Context,
			GPULayers:  gpu,
			Threads:    a.cfg.Threads,
			Embedding:  embedding,
			ExtraArgs:  a.cfg.ExtraArgs,
			LogPrefix:  prefix + "/" + name,
			Log:        a.log,
		})
		if err == nil {
			return srv, name, nil
		}
		a.logf("Runtime %s failed: %v", name, err)
		errs = append(errs, fmt.Errorf("%s: %w", name, err))
		if a.ctx.Err() != nil {
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
	old := a.chat
	a.chat = nil
	a.chatState, a.chatErr = StateStarting, ""
	m, err := a.pickChatModel()
	if err != nil {
		a.chatState, a.chatErr = StateNoModel, err.Error()
		a.engMu.Unlock()
		old.Stop()
		return
	}
	a.chatModel = m
	a.engMu.Unlock()
	old.Stop()

	srv, rt, err := a.startServer(m, false)

	a.engMu.Lock()
	defer a.engMu.Unlock()
	if gen != a.engGen || a.ctx.Err() != nil {
		srv.Stop() // superseded by a newer start or shut down
		return
	}
	if err != nil {
		a.chatState, a.chatErr = StateError, err.Error()
		return
	}
	a.chat, a.chatRuntime, a.chatState = srv, rt, StateReady
	a.logf("Chat model ready: %s (%s)", m.Name, rt)
}

func (a *App) startEmbed() {
	models := a.cfg.ModelsByRole("embedding")
	var m *config.Model
	for i := range models {
		if a.cfg.Present(models[i]) {
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

	srv, _, err := a.startServer(*m, true)

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
		chat, embed := a.chat, a.embed
		a.chat, a.embed = nil, nil
		a.engMu.Unlock()
		chat.Stop()
		embed.Stop()
	})
}
