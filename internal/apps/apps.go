// Package apps installs and runs the Pro edition's add-on programs. Each app
// is a set of npm packages run with the drive's own Node.js
// (runtime/node/<slug>), installed per kind of computer into
// apps/<id>/<slug>, because npm packages carry native parts and browsers
// built for one system. Everything an app writes goes to the drive: its
// home, cache and temp folders are redirected there.
package apps

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ninovee/usbai/internal/config"
)

// NodeVersion is the Node.js release drivetool puts on the drive.
const NodeVersion = "v24.22.0"

// App states reported to the UI.
const (
	NotInstalled = "not_installed"
	Installing   = "installing"
	Installed    = "installed" // ready; a server app that is stopped
	Starting     = "starting"
	Running      = "running"
	Failed       = "error"
)

// Manager installs, starts and stops the apps of one drive.
type Manager struct {
	root string // drive root
	slug string // this computer's kind, e.g. "macos-arm64"
	log  io.Writer

	mu   sync.Mutex
	apps map[string]*app

	// RunEnv adds settings when an app starts (e.g. the AI provider and key
	// the user chose); it is not used while installing.
	RunEnv func(id string) map[string]string
}

type app struct {
	cfg    config.App
	state  string
	err    string
	lines  []string // recent output, for the UI
	cmd    *exec.Cmd
	done   chan struct{}
	port   int
	key    string // bearer key the running server requires
	cancel context.CancelFunc
}

// NewManager prepares the apps listed in cfg for this computer.
func NewManager(cfg config.Config, slug string, log io.Writer) *Manager {
	m := &Manager{root: cfg.Root, slug: slug, log: log, apps: map[string]*app{}}
	for _, c := range cfg.Apps {
		a := &app{cfg: c, state: NotInstalled}
		if m.installed(c.ID) {
			a.state = Installed
		}
		m.apps[c.ID] = a
	}
	return m
}

// Status describes one app for the UI.
type Status struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Homepage    string   `json:"homepage,omitempty"`
	Kind        string   `json:"kind"`
	SizeMB      int      `json:"size_mb,omitempty"`
	State       string   `json:"state"`
	Error       string   `json:"error,omitempty"`
	OtherKinds  []string `json:"other_kinds,omitempty"` // computers it is installed for besides this one
	// Missing says what the drive lacks to install the app here.
	Missing string `json:"missing,omitempty"`
	// OpenURL is a running app's own web page.
	OpenURL string `json:"open_url,omitempty"`
	HasPage bool   `json:"has_page,omitempty"`
	Shared  bool   `json:"shared,omitempty"`
}

// List reports every app, in config order.
func (m *Manager) List(order []config.App) []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Status
	for _, c := range order {
		a := m.apps[c.ID]
		if a == nil {
			continue
		}
		st := Status{ID: c.ID, Name: c.Name, Description: c.Description, Homepage: c.Homepage, Kind: c.Kind,
			SizeMB: c.SizeMB, State: a.state, Error: a.err, Shared: c.Shared, HasPage: c.Open != ""}
		if err := m.ready(c); err != nil {
			st.Missing = err.Error()
		}
		if a.state == Running && c.Open != "" {
			st.OpenURL = fmt.Sprintf("http://127.0.0.1:%d%s", a.port, c.Open)
		}
		ents, _ := os.ReadDir(filepath.Join(m.root, "apps", c.ID))
		for _, e := range ents {
			if !c.Shared && e.IsDir() && e.Name() != m.slug && e.Name() != "data" && fileExists(filepath.Join(m.root, "apps", c.ID, e.Name(), markerFile)) {
				st.OtherKinds = append(st.OtherKinds, e.Name())
			}
		}
		out = append(out, st)
	}
	return out
}

// Log returns an app's recent output.
func (m *Manager) Log(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return "", errUnknown(id)
	}
	return strings.Join(a.lines, "\n"), nil
}

func errUnknown(id string) error { return fmt.Errorf("no app %q on this drive", id) }

const markerFile = ".installed.json"

// dir is where an app is installed for this computer: its own folder per
// kind of computer, or one shared folder.
func (m *Manager) dir(id string) string {
	if a, ok := m.apps[id]; ok && a.cfg.Shared {
		return filepath.Join(m.root, "apps", id, "shared")
	}
	return filepath.Join(m.root, "apps", id, m.slug)
}
func (m *Manager) dataDir(id string) string { return filepath.Join(m.root, "apps", id, "data") }
func (m *Manager) installed(id string) bool { return fileExists(filepath.Join(m.dir(id), markerFile)) }
func (m *Manager) nodeDir() string          { return filepath.Join(m.root, "runtime", "node", m.slug) }
func (m *Manager) pythonDir() string        { return filepath.Join(m.root, "runtime", "python", m.slug) }

// PythonBin is where the drive's Python is for slug.
func PythonBin(pyDir, slug string) string {
	if strings.HasPrefix(slug, "windows") {
		return filepath.Join(pyDir, "python.exe")
	}
	return filepath.Join(pyDir, "bin", "python3")
}

// needs reports which runtimes an app uses.
func needs(c config.App) (node, python bool) {
	node = len(c.Packages) > 0 || (c.Entry != "" && c.Runtime != "python")
	python = len(c.Pip) > 0 || c.Runtime == "python"
	for _, st := range c.Steps {
		switch st.Run {
		case "python":
			python = true
		default:
			node = true
		}
	}
	return node, python
}

// ready reports what the drive lacks to install c on this computer.
func (m *Manager) ready(c config.App) error {
	if c.Kind == "installer" {
		if c.Downloads[m.slug] == "" {
			return fmt.Errorf("%s has no installer for %s", c.Name, m.slug)
		}
		return nil
	}
	node, python := needs(c)
	if node {
		if err := m.nodeReady(); err != nil {
			return err
		}
	}
	if python && !fileExists(PythonBin(m.pythonDir(), m.slug)) {
		return fmt.Errorf("Python for %s is not on this drive yet — run: go run ./cmd/drivetool fetch-python -drive <drive>", m.slug)
	}
	return nil
}

// NodeBin and NpmCLI are where the drive's Node.js and npm are for slug.
func NodeBin(nodeDir, slug string) string {
	if strings.HasPrefix(slug, "windows") {
		return filepath.Join(nodeDir, "node.exe")
	}
	return filepath.Join(nodeDir, "bin", "node")
}

func NpmCLI(nodeDir, slug string) string {
	if strings.HasPrefix(slug, "windows") {
		return filepath.Join(nodeDir, "node_modules", "npm", "bin", "npm-cli.js")
	}
	return filepath.Join(nodeDir, "lib", "node_modules", "npm", "bin", "npm-cli.js")
}

func (m *Manager) nodeReady() error {
	if !fileExists(NodeBin(m.nodeDir(), m.slug)) || !fileExists(NpmCLI(m.nodeDir(), m.slug)) {
		return fmt.Errorf("Node.js for %s is not on this drive yet — run: go run ./cmd/drivetool fetch-node -drive <drive>", m.slug)
	}
	return nil
}

// Install downloads and sets up an app for this kind of computer, in the
// background. It needs the internet.
func (m *Manager) Install(id string) error {
	m.mu.Lock()
	a, ok := m.apps[id]
	if !ok {
		m.mu.Unlock()
		return errUnknown(id)
	}
	if err := m.ready(a.cfg); err != nil {
		m.mu.Unlock()
		return err
	}
	switch a.state {
	case Installing:
		m.mu.Unlock()
		return nil
	case Running, Starting:
		m.mu.Unlock()
		return errors.New(a.cfg.Name + " is running — stop it first")
	}
	a.state, a.err, a.lines = Installing, "", nil
	m.mu.Unlock()
	go func() {
		err := m.install(a)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			a.state, a.err = Failed, err.Error()
			if m.installed(id) {
				a.state = Installed // the previous install still works
			}
			m.addLine(a, "✗ "+err.Error())
			return
		}
		a.state = Installed
		m.addLine(a, "✓ Installed.")
	}()
	return nil
}

// install runs npm in a temporary folder on this computer (the drive's
// exFAT can't hold the links npm makes while installing), then copies the
// result to the drive, turning links into plain files.
func (m *Manager) install(a *app) error {
	c := a.cfg
	tmp, err := os.MkdirTemp("", "gangai-app-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	env := m.env(c, tmp, 0, "")
	env = append(env, "npm_config_cache="+filepath.Join(tmp, "home", ".npm"), "npm_config_update_notifier=false",
		"PIP_CACHE_DIR="+filepath.Join(tmp, "home", ".cache", "pip"), "PIP_DISABLE_PIP_VERSION_CHECK=1")
	if c.Source != "" {
		m.say(a, "Downloading the source …")
		if err := downloadExtract(c.Source, filepath.Join(tmp, c.SourceDir)); err != nil {
			return fmt.Errorf("download %s: %w", c.Source, err)
		}
	}
	if c.Kind == "installer" {
		u := c.Downloads[m.slug]
		m.say(a, "Downloading "+u+" …")
		if err := downloadFile(u, filepath.Join(tmp, path.Base(u))); err != nil {
			return fmt.Errorf("download: %w", err)
		}
	}
	node := NodeBin(m.nodeDir(), m.slug)
	npm := NpmCLI(m.nodeDir(), m.slug)
	py := PythonBin(m.pythonDir(), m.slug)
	if len(c.Packages) > 0 {
		if !fileExists(filepath.Join(tmp, "package.json")) {
			if err := os.WriteFile(filepath.Join(tmp, "package.json"), []byte(`{"name":"gangai-app","private":true}`+"\n"), 0o644); err != nil {
				return err
			}
		}
		args := append([]string{npm, "install", "--no-audit", "--no-fund", "--omit=dev", "--loglevel=error"}, c.Packages...)
		m.say(a, "Downloading "+strings.Join(c.Packages, ", ")+" …")
		if err := m.runLogged(a, tmp, env, node, args...); err != nil {
			return fmt.Errorf("npm install failed: %w", err)
		}
	}
	if len(c.Pip) > 0 {
		args := append([]string{"-m", "pip", "install", "--target", filepath.Join(tmp, "py"), "--no-warn-script-location", "--progress-bar", "off"}, c.Pip...)
		m.say(a, "Downloading "+strings.Join(c.Pip, ", ")+" (Python) …")
		if err := m.runLogged(a, tmp, env, py, args...); err != nil {
			return fmt.Errorf("pip install failed: %w", err)
		}
	}
	for _, st := range c.Steps {
		dir := filepath.Join(tmp, st.Dir)
		m.say(a, "Setting up: "+st.Run+" "+strings.Join(st.Args, " "))
		var err error
		switch st.Run {
		case "npm":
			err = m.runLogged(a, dir, env, node, append([]string{npm}, st.Args...)...)
		case "python":
			err = m.runLogged(a, dir, env, py, st.Args...)
		default: // a node script, relative to the install folder
			if len(st.Args) == 0 {
				continue
			}
			err = m.runLogged(a, dir, env, node, append([]string{filepath.Join(tmp, st.Args[0])}, st.Args[1:]...)...)
		}
		if err != nil {
			return fmt.Errorf("setup step %q failed: %w", st.Run+" "+strings.Join(st.Args, " "), err)
		}
	}
	m.say(a, "Copying to the drive …")
	dest := m.dir(c.ID)
	partial := dest + ".partial"
	os.RemoveAll(partial)
	skip := func(rel string) bool {
		base := filepath.Base(rel)
		return base == ".bin" || base == "__pycache__" || rel == filepath.Join("home", ".npm") ||
			rel == filepath.Join("home", ".cache", "pip") || rel == "tmp"
	}
	if err := CopyTree(tmp, partial, skip); err != nil {
		os.RemoveAll(partial)
		return fmt.Errorf("copy to the drive: %w", err)
	}
	marker, _ := json.Marshal(map[string]any{"packages": c.Packages, "pip": c.Pip, "source": c.Source, "installed": time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(partial, markerFile), marker, 0o644); err != nil {
		return err
	}
	os.RemoveAll(dest)
	return os.Rename(partial, dest)
}

// env builds an app's environment: the drive's Node first on PATH, and
// home, cache and temp folders inside dir so nothing lands on the computer.
func (m *Manager) env(c config.App, dir string, port int, key string) []string {
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	os.MkdirAll(home, 0o755)
	os.MkdirAll(tmp, 0o755)
	os.MkdirAll(m.dataDir(c.ID), 0o755)
	nodeBinDir := filepath.Dir(NodeBin(m.nodeDir(), m.slug))
	pyBinDir := filepath.Dir(PythonBin(m.pythonDir(), m.slug))
	sep := string(os.PathListSeparator)
	set := map[string]string{
		"PATH":             nodeBinDir + sep + pyBinDir + sep + os.Getenv("PATH"),
		"PYTHONPATH":       filepath.Join(dir, "py"),
		"PYTHONNOUSERSITE": "1",
		"PYTHONUTF8":       "1",
		"HOME":             home,
		"USERPROFILE":      home,
		"APPDATA":          filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA":     filepath.Join(home, "AppData", "Local"),
		"XDG_CACHE_HOME":   filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME":  filepath.Join(home, ".config"),
		"XDG_DATA_HOME":    filepath.Join(home, ".local", "share"),
		"TMPDIR":           tmp,
		"TEMP":             tmp,
		"TMP":              tmp,
		"DO_NOT_TRACK":     "1",
	}
	if port > 0 {
		set["PORT"] = fmt.Sprint(port)
	}
	exe, npmPlatform := "", npmPlatformOf(m.slug)
	if strings.HasPrefix(m.slug, "windows") {
		exe = ".exe"
	}
	repl := strings.NewReplacer("${APP_DIR}", dir, "${DATA}", m.dataDir(c.ID), "${PORT}", fmt.Sprint(port),
		"${KEY}", key, "${NODE_DIR}", m.nodeDir(), "${EXE}", exe, "${NPM_PLATFORM}", npmPlatform)
	for k, v := range c.Env {
		set[k] = repl.Replace(v)
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; ok || strings.EqualFold(k, "PATH") && runtime.GOOS == "windows" {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}

// npmPlatformOf turns a drive folder name (macos-arm64) into npm's
// platform name (darwin-arm64), which some packages use in their paths.
func npmPlatformOf(slug string) string {
	os, arch, _ := strings.Cut(slug, "-")
	switch os {
	case "macos":
		os = "darwin"
	case "windows":
		os = "win32"
	}
	return os + "-" + arch
}

// runLogged runs a command to completion, adding its output to the log.
func (m *Manager) runLogged(a *app, dir string, env []string, name string, args ...string) error {
	cmd := command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	go m.copyLines(a, pr)
	err := cmd.Wait()
	pw.Close()
	if err != nil {
		if tail := m.lastLine(a); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
	}
	return err
}

func (m *Manager) copyLines(a *app, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		m.mu.Lock()
		m.addLine(a, sc.Text())
		m.mu.Unlock()
	}
	io.Copy(io.Discard, r)
}

func (m *Manager) say(a *app, line string) {
	m.mu.Lock()
	m.addLine(a, line)
	m.mu.Unlock()
}

// addLine keeps the last lines of output; m.mu must be held.
func (m *Manager) addLine(a *app, line string) {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return
	}
	fmt.Fprintf(m.log, "[%s] %s\n", a.cfg.ID, line)
	a.lines = append(a.lines, line)
	if len(a.lines) > 300 {
		a.lines = a.lines[len(a.lines)-300:]
	}
}

func (m *Manager) lastLine(a *app) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(a.lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(a.lines[i]); l != "" && !strings.HasPrefix(l, "Setting up") && !strings.HasPrefix(l, "Downloading") {
			return l
		}
	}
	return ""
}

// Start runs a server app and waits until it answers.
func (m *Manager) Start(ctx context.Context, id string) error {
	m.mu.Lock()
	a, ok := m.apps[id]
	if !ok {
		m.mu.Unlock()
		return errUnknown(id)
	}
	if a.cfg.Kind != "server" {
		m.mu.Unlock()
		return errors.New(a.cfg.Name + " is not a program that keeps running; agents use it directly")
	}
	switch {
	case a.state == Running:
		m.mu.Unlock()
		return nil
	case a.state == Starting:
		done := a.done
		m.mu.Unlock()
		return m.waitReady(ctx, a, done)
	case !m.installed(id):
		m.mu.Unlock()
		return errors.New(a.cfg.Name + " is not installed for this computer yet — install it in the Apps tab")
	}
	port, err := freePort()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	dir := m.dir(id)
	// Each run starts clean: browsing data from the last run (kept under
	// tmp, see the app's env) is not kept on the drive.
	os.RemoveAll(filepath.Join(dir, "tmp"))
	key := randomKey()
	name, args := m.entry(a.cfg, dir, port)
	cmd := command(name, args...)
	cmd.Dir, cmd.Env = dir, m.withRunEnv(id, m.env(a.cfg, dir, port, key))
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	startGroup(cmd)
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		pw.Close()
		return err
	}
	done := make(chan struct{})
	a.cmd, a.done, a.port, a.key, a.state, a.err = cmd, done, port, key, Starting, ""
	m.addLine(a, fmt.Sprintf("Starting %s on 127.0.0.1:%d …", a.cfg.Name, port))
	m.mu.Unlock()
	go m.copyLines(a, pr)
	go func() {
		err := cmd.Wait()
		pw.Close()
		m.mu.Lock()
		if a.cmd == cmd {
			a.cmd = nil
			if a.state == Running || a.state == Starting {
				a.state = Failed
				a.err = "stopped unexpectedly"
				if err != nil {
					a.err += ": " + err.Error()
				}
			}
		}
		m.mu.Unlock()
		close(done)
	}()
	return m.waitReady(ctx, a, done)
}

// waitReady polls the app's health address until it answers.
func (m *Manager) waitReady(ctx context.Context, a *app, done chan struct{}) error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		m.mu.Lock()
		state, port, key, health := a.state, a.port, a.key, a.cfg.Health
		m.mu.Unlock()
		if state == Running {
			return nil
		}
		if state != Starting {
			return fmt.Errorf("%s did not start: %s", a.cfg.Name, m.lastLine(a))
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, health), nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				m.mu.Lock()
				if a.state == Starting {
					a.state = Running
					m.addLine(a, "✓ Running.")
				}
				m.mu.Unlock()
				return nil
			}
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			m.Stop(a.cfg.ID)
			return fmt.Errorf("%s did not answer within 3 minutes", a.cfg.Name)
		}
	}
}

// Stop ends a running server app and the programs it started.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	a, ok := m.apps[id]
	if !ok {
		m.mu.Unlock()
		return errUnknown(id)
	}
	cmd, done := a.cmd, a.done
	a.cmd = nil
	if a.state == Running || a.state == Starting || a.state == Failed {
		a.state, a.err = NotInstalled, ""
		if m.installed(id) {
			a.state = Installed
		}
	}
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	killGroup(cmd)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	m.say(a, "Stopped.")
	return nil
}

// StopAll stops every running app (on shutdown).
func (m *Manager) StopAll() {
	m.mu.Lock()
	var ids []string
	for id, a := range m.apps {
		if a.cmd != nil {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}

// Endpoint returns a running server app's address and bearer key,
// starting the app first if it is installed but stopped.
func (m *Manager) Endpoint(ctx context.Context, id string) (string, string, error) {
	if err := m.Start(ctx, id); err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.apps[id]
	return fmt.Sprintf("http://127.0.0.1:%d", a.port), a.key, nil
}

// Dir is where an app is installed for this computer.
func (m *Manager) Dir(id string) string { return m.dir(id) }

// Installed reports whether an app is installed for this computer.
func (m *Manager) Installed(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.apps[id]
	return ok && m.installed(id)
}

// RunTool runs a tool app once with args in dir and returns its output.
func (m *Manager) RunTool(ctx context.Context, id, dir string, args ...string) (string, error) {
	m.mu.Lock()
	a, ok := m.apps[id]
	m.mu.Unlock()
	if !ok {
		return "", errUnknown(id)
	}
	if !m.Installed(id) {
		return "", errors.New(a.cfg.Name + " is not installed for this computer yet — install it in the Apps tab")
	}
	appDir := m.dir(id)
	name, base := m.entry(a.cfg, appDir, 0)
	cmd := commandContext(ctx, name, append(base, args...)...)
	cmd.Dir, cmd.Env = dir, m.withRunEnv(id, m.env(a.cfg, appDir, 0, ""))
	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > 16000 {
		text = "…" + text[len(text)-16000:]
	}
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		m.say(a, l)
	}
	if ctx.Err() != nil {
		return text, ctx.Err()
	}
	return text, err
}

func (m *Manager) withRunEnv(id string, env []string) []string {
	if m.RunEnv == nil {
		return env
	}
	for k, v := range m.RunEnv(id) {
		env = append(env, k+"="+v)
	}
	return env
}

// entry is the command that runs an app: node with its script, or python
// with its module. ${PORT} in Args is filled in.
func (m *Manager) entry(c config.App, dir string, port int) (string, []string) {
	var args []string
	for _, x := range c.Args {
		args = append(args, strings.ReplaceAll(x, "${PORT}", fmt.Sprint(port)))
	}
	if c.Runtime == "python" {
		return PythonBin(m.pythonDir(), m.slug), append([]string{"-m", c.Entry}, args...)
	}
	return NodeBin(m.nodeDir(), m.slug), append([]string{filepath.Join(dir, c.Entry)}, args...)
}

// OpenInstaller opens the installer an "installer" app downloaded for
// this computer, with the computer's own handler.
func (m *Manager) OpenInstaller(id string) error {
	m.mu.Lock()
	a, ok := m.apps[id]
	m.mu.Unlock()
	if !ok {
		return errUnknown(id)
	}
	u := a.cfg.Downloads[m.slug]
	if a.cfg.Kind != "installer" || u == "" || !m.Installed(id) {
		return errors.New(a.cfg.Name + " has not been downloaded for this computer yet")
	}
	p := filepath.Join(m.dir(id), path.Base(u))
	if strings.HasSuffix(p, ".run") || strings.HasSuffix(p, ".bin") {
		os.Chmod(p, 0o755)
	}
	m.say(a, "Opening "+p)
	return openFile(p)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func randomKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// CopyTree copies src to dst, following symbolic links (so the copy holds
// plain files, as exFAT needs) and leaving out paths skip matches. Broken
// links are left out.
func CopyTree(src, dst string, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel != "." && skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		st, err := os.Stat(p) // follows links
		if err != nil {
			return nil // broken link
		}
		if st.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				// A link to a folder: copy what it points to.
				return CopyTree(p, target, nil)
			}
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(p, target, st.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
