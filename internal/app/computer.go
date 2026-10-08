package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ninovee/usbai/internal/rag"
)

// Computer access lets agents act on this computer: open apps, files and
// websites, work with files in one workspace folder, run the user's
// Shortcuts (macOS) and, if allowed, terminal commands. It is off by
// default. Every action that changes anything waits for the user to press
// Allow in the chat; reading inside the workspace does not ask. An agent
// can't have both web and computer tools, so a web page can't steer an
// agent into running something here.

// ComputerSettings is stored encrypted in the vault.
type ComputerSettings struct {
	Enabled   bool   `json:"enabled"`
	Workspace string `json:"workspace"` // folder the file tools work in
	Terminal  bool   `json:"terminal"`  // allow run_command
}

var computerTools = map[string]bool{
	"open_item": true, "list_files": true, "read_file": true, "write_file": true,
	"list_shortcuts": true, "run_shortcut": true, "run_command": true,
}

const (
	actionTimeout  = 10 * time.Minute // unanswered approval requests are denied
	commandTimeout = 2 * time.Minute
	maxWriteBytes  = 1 << 20
)

func defaultWorkspace() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Documents", "Private AI Workspace")
}

// ComputerSettings returns the saved settings (off by default).
func (a *App) ComputerSettings() (ComputerSettings, error) {
	cs := ComputerSettings{Workspace: defaultWorkspace()}
	v, err := a.unlocked()
	if err != nil {
		return cs, err
	}
	var saved ComputerSettings
	if v.GetJSON("computer_settings", &saved) == nil {
		cs.Enabled, cs.Terminal = saved.Enabled, saved.Terminal
		if saved.Workspace != "" {
			cs.Workspace = saved.Workspace
		}
	}
	return cs, nil
}

// SaveComputerSettings stores the settings; the workspace must be an
// absolute folder, which is created if needed.
func (a *App) SaveComputerSettings(cs ComputerSettings) (ComputerSettings, error) {
	v, err := a.unlocked()
	if err != nil {
		return cs, err
	}
	cs.Workspace = strings.TrimSpace(cs.Workspace)
	if strings.HasPrefix(cs.Workspace, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cs.Workspace = filepath.Join(home, cs.Workspace[2:])
		}
	}
	if cs.Workspace == "" {
		cs.Workspace = defaultWorkspace()
	}
	if !filepath.IsAbs(cs.Workspace) {
		return cs, errors.New("the workspace must be a full folder path, e.g. ~/Documents/Private AI Workspace")
	}
	cs.Workspace = filepath.Clean(cs.Workspace)
	if home, err := os.UserHomeDir(); err == nil && (cs.Workspace == home || cs.Workspace == filepath.Dir(home)) {
		return cs, errors.New("choose a dedicated folder, not your whole home folder")
	}
	if cs.Workspace == filepath.VolumeName(cs.Workspace)+string(filepath.Separator) {
		return cs, errors.New("choose a dedicated folder, not the whole disk")
	}
	if cs.Enabled {
		if err := os.MkdirAll(cs.Workspace, 0o755); err != nil {
			return cs, fmt.Errorf("can't create the workspace folder: %w", err)
		}
	}
	return cs, v.PutJSON("computer_settings", cs)
}

func (a *App) computerEnabled() bool {
	cs, err := a.ComputerSettings()
	return err == nil && cs.Enabled
}

func (a *App) terminalEnabled() bool {
	cs, err := a.ComputerSettings()
	return err == nil && cs.Enabled && cs.Terminal
}

// ---- Approval ----

// Action is a request shown to the user before an agent changes anything.
type Action struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Title  string `json:"title"`  // e.g. Open “Safari”
	Detail string `json:"detail"` // the exact command, file content, …
}

type approvals struct {
	mu      sync.Mutex
	waiting map[string]chan bool
}

// askApproval shows the action in the chat and waits for Allow or Deny.
func (a *App) askApproval(ctx context.Context, ev ChatEvents, act Action) (bool, error) {
	if ev == nil {
		return false, nil
	}
	act.ID = newID()
	ch := make(chan bool, 1)
	a.actions.mu.Lock()
	if a.actions.waiting == nil {
		a.actions.waiting = map[string]chan bool{}
	}
	a.actions.waiting[act.ID] = ch
	a.actions.mu.Unlock()
	defer func() {
		a.actions.mu.Lock()
		delete(a.actions.waiting, act.ID)
		a.actions.mu.Unlock()
	}()
	if err := ev.Approve(act); err != nil {
		return false, err
	}
	select {
	case ok := <-ch:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(actionTimeout):
		return false, nil
	}
}

// AnswerAction records the user's Allow (true) or Deny for a waiting action.
func (a *App) AnswerAction(id string, allow bool) error {
	a.actions.mu.Lock()
	ch, ok := a.actions.waiting[id]
	a.actions.mu.Unlock()
	if !ok {
		return errors.New("this request is no longer waiting (it was answered, timed out or the chat stopped)")
	}
	select {
	case ch <- allow:
	default:
	}
	return nil
}

const deniedText = "The user denied this action. Do not try it again; ask the user what they would like instead."

// ---- Tools ----

func (a *App) runComputerTool(ctx context.Context, ev ChatEvents, name string, arg func(string) string) string {
	cs, err := a.ComputerSettings()
	if err != nil || !cs.Enabled {
		return "Error: computer access is off — the user can turn it on in Agents → Computer access."
	}
	ask := func(title, detail string) (bool, string) {
		ok, err := a.askApproval(ctx, ev, Action{Tool: name, Title: title, Detail: detail})
		if err != nil {
			return false, "Error: " + err.Error()
		}
		if !ok {
			return false, deniedText
		}
		return true, ""
	}
	switch name {
	case "open_item":
		target := arg("target")
		if target == "" {
			return "Error: say what to open (an app name, a file path or a web address)."
		}
		if ok, msg := ask(fmt.Sprintf("Open “%s”", target), openDescription(target)); !ok {
			return msg
		}
		if err := openItem(ctx, target, cs.Workspace); err != nil {
			return "Error: " + err.Error()
		}
		return "Opened " + target + "."

	case "list_files":
		dir, err := inWorkspace(cs.Workspace, arg("path"))
		if err != nil {
			return "Error: " + err.Error()
		}
		return listFiles(cs.Workspace, dir)

	case "read_file":
		p, err := inWorkspace(cs.Workspace, arg("path"))
		if err != nil {
			return "Error: " + err.Error()
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "Error: " + friendlyFSError(err)
		}
		text, err := rag.Extract(filepath.Base(p), data)
		if errors.Is(err, rag.ErrUnsupported) {
			text, err = string(bytes.ToValidUTF8(data, []byte("�"))), nil
		}
		if err != nil {
			return "Error: " + err.Error()
		}
		return text

	case "write_file":
		rel, content := arg("path"), arg("content")
		p, err := inWorkspace(cs.Workspace, rel)
		if err != nil {
			return "Error: " + err.Error()
		}
		if len(content) > maxWriteBytes {
			return "Error: the content is too long (1 MB at most)."
		}
		verb := "Create"
		if _, err := os.Stat(p); err == nil {
			verb = "Replace"
		}
		preview := content
		if r := []rune(preview); len(r) > 800 {
			preview = string(r[:800]) + "\n…"
		}
		if ok, msg := ask(fmt.Sprintf("%s the file “%s” in your workspace", verb, relTo(cs.Workspace, p)), preview); !ok {
			return msg
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "Error: " + friendlyFSError(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "Error: " + friendlyFSError(err)
		}
		return fmt.Sprintf("Saved %s (%d characters).", relTo(cs.Workspace, p), len([]rune(content)))

	case "list_shortcuts":
		if runtime.GOOS != "darwin" {
			return "Error: Shortcuts are only available on a Mac."
		}
		out, err := run(ctx, 30*time.Second, "", "shortcuts", "list")
		if err != nil {
			return "Error: " + err.Error()
		}
		if strings.TrimSpace(out) == "" {
			return "The user has no Shortcuts yet. They can make some in the Shortcuts app."
		}
		return "The user's Shortcuts:\n" + out

	case "run_shortcut":
		if runtime.GOOS != "darwin" {
			return "Error: Shortcuts are only available on a Mac."
		}
		sc, input := arg("name"), arg("input")
		if sc == "" {
			return "Error: give the Shortcut's name (use list_shortcuts to see them)."
		}
		detail := "Shortcut: " + sc
		if input != "" {
			detail += "\nInput: " + input
		}
		if ok, msg := ask(fmt.Sprintf("Run the Shortcut “%s”", sc), detail); !ok {
			return msg
		}
		return runShortcut(ctx, sc, input)

	case "run_command":
		if !cs.Terminal {
			return "Error: terminal commands are off — the user can allow them in Agents → Computer access."
		}
		cmdline := arg("command")
		if cmdline == "" {
			return "Error: give the command to run."
		}
		if ok, msg := ask("Run a terminal command", cmdline+"\n\n(in "+cs.Workspace+")"); !ok {
			return msg
		}
		if err := os.MkdirAll(cs.Workspace, 0o755); err != nil {
			return "Error: " + err.Error()
		}
		var out string
		if runtime.GOOS == "windows" {
			out, err = run(ctx, commandTimeout, cs.Workspace, "cmd", "/C", cmdline)
		} else {
			out, err = run(ctx, commandTimeout, cs.Workspace, "/bin/sh", "-c", cmdline)
		}
		if err != nil {
			return fmt.Sprintf("Error: %v\nOutput:\n%s", err, out)
		}
		if strings.TrimSpace(out) == "" {
			return "Done (no output)."
		}
		return out
	}
	return "Error: unknown tool"
}

// inWorkspace resolves a path inside the workspace and refuses anything
// that would leave it (.., absolute paths elsewhere, symbolic links out).
func inWorkspace(ws, rel string) (string, error) {
	if ws == "" {
		return "", errors.New("no workspace folder is set")
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(ws)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	var p string
	if filepath.IsAbs(rel) {
		p = filepath.Clean(rel)
		if real, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			p = filepath.Join(real, filepath.Base(p))
		}
	} else {
		p = filepath.Join(root, filepath.Clean(string(filepath.Separator)+rel))
	}
	// Follow links in what exists of the path, then check it stays inside.
	check := p
	for {
		if real, err := filepath.EvalSymlinks(check); err == nil {
			rest, _ := filepath.Rel(check, p)
			check = filepath.Join(real, rest)
			break
		}
		parent := filepath.Dir(check)
		if parent == check {
			break
		}
		check = parent
	}
	if check != root && !strings.HasPrefix(check, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the workspace folder; only files in %s can be used", rel, ws)
	}
	return check, nil
}

func relTo(ws, p string) string {
	if root, err := filepath.EvalSymlinks(ws); err == nil {
		if r, err := filepath.Rel(root, p); err == nil {
			return filepath.ToSlash(r)
		}
	}
	return filepath.Base(p)
}

func listFiles(ws, dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "Error: " + friendlyFSError(err)
	}
	if len(ents) == 0 {
		return "The folder " + relTo(ws, dir) + " is empty."
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var b strings.Builder
	fmt.Fprintf(&b, "Files in %s:\n", relTo(ws, dir))
	for i, e := range ents {
		if i == 200 {
			fmt.Fprintf(&b, "… and %d more\n", len(ents)-200)
			break
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			fmt.Fprintf(&b, "- %s/ (folder)\n", e.Name())
		} else if info, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "- %s (%d KB)\n", e.Name(), (info.Size()+1023)/1024)
		}
	}
	return b.String()
}

func friendlyFSError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "no such file or folder in the workspace"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}
	return err.Error()
}

func openDescription(target string) string {
	switch {
	case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"):
		return "Opens this web address in your browser."
	case strings.ContainsAny(target, `/\`):
		return "Opens this file or folder with its usual app."
	}
	return "Opens this app."
}

// openItem opens an app (by name), a file or folder, or a web address.
func openItem(ctx context.Context, target, ws string) error {
	isURL := strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
	path := target
	if !isURL && !filepath.IsAbs(path) && strings.ContainsAny(path, `/\`) {
		path = filepath.Join(ws, path) // relative paths are in the workspace
	}
	_, statErr := os.Stat(path)
	isPath := !isURL && statErr == nil
	var err error
	switch runtime.GOOS {
	case "darwin":
		switch {
		case isURL:
			_, err = run(ctx, 30*time.Second, "", "open", target)
		case isPath:
			_, err = run(ctx, 30*time.Second, "", "open", path)
		default:
			_, err = run(ctx, 30*time.Second, "", "open", "-a", target)
		}
	case "windows":
		t := target
		if isPath {
			t = path
		}
		_, err = run(ctx, 30*time.Second, "", "cmd", "/C", "start", "", t)
	default:
		if isURL || isPath {
			t := target
			if isPath {
				t = path
			}
			_, err = run(ctx, 30*time.Second, "", "xdg-open", t)
		} else {
			cmd := exec.Command(target)
			err = cmd.Start()
			if err == nil {
				go cmd.Wait()
			}
		}
	}
	if err != nil && !isURL && !isPath {
		return fmt.Errorf("couldn't open an app called %q (%v)", target, err)
	}
	return err
}

func runShortcut(ctx context.Context, name, input string) string {
	dir, err := os.MkdirTemp("", "privateai-shortcut")
	if err != nil {
		return "Error: " + err.Error()
	}
	defer os.RemoveAll(dir)
	outPath := filepath.Join(dir, "output.txt")
	args := []string{"run", name, "--output-path", outPath}
	if input != "" {
		inPath := filepath.Join(dir, "input.txt")
		if err := os.WriteFile(inPath, []byte(input), 0o600); err != nil {
			return "Error: " + err.Error()
		}
		args = append(args, "--input-path", inPath)
	}
	if out, err := run(ctx, commandTimeout, "", "shortcuts", args...); err != nil {
		return fmt.Sprintf("Error: the Shortcut failed: %v %s", err, out)
	}
	if b, err := os.ReadFile(outPath); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return "The Shortcut finished. Its output:\n" + string(b)
	}
	return "The Shortcut finished."
}

// run executes a program with a time limit and returns its combined output
// (at most 8 KB).
func run(ctx context.Context, limit time.Duration, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := buf.String()
	if len(out) > 8<<10 {
		out = out[:8<<10] + "\n…(output cut)"
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("stopped after %s", limit)
	}
	return out, err
}
