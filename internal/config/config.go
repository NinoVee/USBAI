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
	SHA256   string `json:"sha256,omitempty"`
	MinRAMGB int    `json:"min_ram_gb,omitempty"`
	Context  int    `json:"context,omitempty"`
	// MMProj is the multimodal projector that lets a chat model see images
	// (drive-relative), with its download URL for drivetool.
	MMProj       string `json:"mmproj,omitempty"`
	MMProjURL    string `json:"mmproj_url,omitempty"`
	MMProjSHA256 string `json:"mmproj_sha256,omitempty"`
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
		SystemPrompt: "You are Private AI, a helpful assistant running entirely offline on the user's own drive. " +
			"Nothing the user says or shares leaves this computer. Be accurate and concise. " +
			"When document excerpts are provided, base your answer on them and cite the document name; " +
			"if they do not contain the answer, say so.",
	}
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
	return cfg, nil
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

// Vision reports whether the model's image projector is on the drive.
func (c Config) Vision(m Model) bool {
	if m.MMProj == "" {
		return false
	}
	st, err := os.Stat(c.Path(m.MMProj))
	return err == nil && !st.IsDir() && st.Size() > 0
}

// Present reports whether the model file exists on the drive.
func (c Config) Present(m Model) bool {
	st, err := os.Stat(c.Path(m.File))
	return err == nil && !st.IsDir() && st.Size() > 0
}
