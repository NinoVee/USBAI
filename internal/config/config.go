// Package config loads the drive's config.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is the name of the config file at the drive root. Its presence is
// also how the launcher finds the drive root.
const FileName = "config.json"

// Model describes one GGUF model that may live on the drive.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Role is "chat" or "embedding".
	Role string `json:"role"`
	// File is relative to the drive root.
	File string `json:"file"`
	// URL is only used by drivetool when preparing a drive.
	URL      string `json:"url,omitempty"`
	Size     int64  `json:"size,omitempty"` // expected bytes; catches truncated copies
	SHA256   string `json:"sha256,omitempty"`
	MinRAMGB int    `json:"min_ram_gb,omitempty"`
	Context  int    `json:"context,omitempty"`
	// MMProj is the multimodal projector that lets a chat model see images
	// (drive-relative), with its download URL for drivetool.
	MMProj       string `json:"mmproj,omitempty"`
	MMProjURL    string `json:"mmproj_url,omitempty"`
	MMProjSize   int64  `json:"mmproj_size,omitempty"`
	MMProjSHA256 string `json:"mmproj_sha256,omitempty"`
	// Extra lists further files the model needs (e.g. the voices of a
	// speech-synthesis model).
	Extra []ExtraFile `json:"extra,omitempty"`
	// Base makes this entry a personality of another model: it uses the
	// base model's files, and Persona is added to the system prompt.
	Base    string `json:"base,omitempty"`
	Persona string `json:"persona,omitempty"`
	// Greeting, when set, is written verbatim as the first line of the
	// model's first reply in a chat, so the catchphrase is exact instead
	// of left to a small model to reproduce.
	Greeting string `json:"greeting,omitempty"`
	// Revision goes up when an entry on the template changes in a way that
	// drives should pick up on update (e.g. a rewritten personality).
	Revision int `json:"revision,omitempty"`
	// Prefixes some embedding models (e.g. nomic-embed) expect.
	QueryPrefix    string `json:"query_prefix,omitempty"`
	DocumentPrefix string `json:"document_prefix,omitempty"`
}

// Config is the drive configuration.
type Config struct {
	ListenPort   int      `json:"listen_port"`
	OpenBrowser  bool     `json:"open_browser"`
	GPULayers    int      `json:"gpu_layers"`
	Threads      int      `json:"threads"`
	ExtraArgs    []string `json:"extra_args,omitempty"`
	SystemPrompt string   `json:"system_prompt"`
	Models       []Model  `json:"models"`

	// Root is the drive root directory; not serialized.
	Root string `json:"-"`
}

// Default returns the built-in defaults, used for any field config.json omits.
func Default() Config {
	return Config{
		ListenPort:  8740,
		OpenBrowser: true,
		GPULayers:   99,
		SystemPrompt: "You are Private AI, a helpful assistant that runs on the user's own drive. " +
			"The user's chats, documents and memory stay on this computer. Be accurate and concise. " +
			"When document excerpts are provided, base your answer on them and cite the document name; " +
			"if they do not contain the answer, say so.",
	}
}

// ExtraFile is one more file of a model. Member, when set, is the path of
// the file inside the .tgz archive at URL.
type ExtraFile struct {
	File   string `json:"file"`
	URL    string `json:"url,omitempty"`
	Member string `json:"member,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Load reads config.json from root.
func Load(root string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", FileName, err)
	}
	cfg.Root = root
	cfg.resolveBases()
	return cfg, nil
}

// resolveBases fills personality entries in with their base model's files.
func (c *Config) resolveBases() {
	byID := map[string]Model{}
	for _, m := range c.Models {
		if m.Base == "" {
			byID[m.ID] = m
		}
	}
	for i, m := range c.Models {
		b, ok := byID[m.Base]
		if m.Base == "" || !ok {
			continue
		}
		m.File, m.URL, m.Size, m.SHA256 = b.File, b.URL, b.Size, b.SHA256
		m.MMProj, m.MMProjURL, m.MMProjSize, m.MMProjSHA256 = b.MMProj, b.MMProjURL, b.MMProjSize, b.MMProjSHA256
		if m.Role == "" {
			m.Role = b.Role
		}
		if m.MinRAMGB == 0 {
			m.MinRAMGB = b.MinRAMGB
		}
		if m.Context == 0 {
			m.Context = b.Context
		}
		c.Models[i] = m
	}
}

// FindRoot walks up from start looking for config.json. The binaries live in
// <root>/bin/<platform>/, so this normally succeeds two levels up.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find " + FileName + " above " + start)
		}
		dir = parent
	}
}

// Path resolves a drive-relative path.
func (c Config) Path(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}
	return filepath.Join(c.Root, filepath.FromSlash(rel))
}

// ModelsByRole returns models with the given role, in config order.
func (c Config) ModelsByRole(role string) []Model {
	var out []Model
	for _, m := range c.Models {
		if m.Role == role {
			out = append(out, m)
		}
	}
	return out
}

// Vision reports whether the model's image projector is on the drive and
// complete.
func (c Config) Vision(m Model) bool {
	return m.MMProj != "" && checkFile(c.Path(m.MMProj), m.MMProjSize) == nil
}

// Check reports why a model's file can't be used: missing, or a different
// size than expected (usually a copy to the drive that was cut short).
func (c Config) Check(m Model) error {
	if err := checkFile(c.Path(m.File), m.Size); err != nil {
		return err
	}
	for _, x := range m.Extra {
		if err := checkFile(c.Path(x.File), x.Size); err != nil {
			return err
		}
	}
	return nil
}

// CheckFile reports why one drive-relative file can't be used.
func (c Config) CheckFile(rel string, size int64) error {
	return checkFile(c.Path(rel), size)
}

func checkFile(path string, want int64) error {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return fmt.Errorf("%s is missing", filepath.Base(path))
	}
	if want > 0 && st.Size() != want {
		return fmt.Errorf("%s is incomplete or damaged (%.2f GB on the drive, should be %.2f GB) — copy it to the drive again",
			filepath.Base(path), float64(st.Size())/(1<<30), float64(want)/(1<<30))
	}
	return nil
}

// Present reports whether the model file exists on the drive.
func (c Config) Present(m Model) bool {
	st, err := os.Stat(c.Path(m.File))
	return err == nil && !st.IsDir() && st.Size() > 0
}
