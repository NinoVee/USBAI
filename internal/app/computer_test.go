package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func toolResults(tr turn) []string {
	var out []string
	for _, raw := range tr.events["tool_result"] {
		var s ToolStep
		json.Unmarshal(raw, &s)
		out = append(out, s.Result)
	}
	return out
}

func TestComputerAccess(t *testing.T) {
	ts := newTestServer(t)
	a := ts.a
	ws := filepath.Join(t.TempDir(), "workspace")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o644)

	var ag Agent
	ts.call("POST", "/api/agents", Agent{Name: "Helper", Knowledge: "none",
		Tools: []string{"open_item", "list_files", "read_file", "write_file", "run_command"}}, &ag)

	// Off by default: no computer tools are offered, and calls are refused.
	if tools := a.agentTools(&ag); len(tools) != 0 {
		t.Fatalf("computer tools offered while off: %v", tools)
	}
	if out := a.runTool(t.Context(), nil, &ag, toolCall("list_files", `{}`)); !strings.Contains(out, "computer access is off") {
		t.Fatalf("off: %q", out)
	}

	if _, err := a.SaveComputerSettings(ComputerSettings{Enabled: true, Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	// Terminal commands stay hidden until allowed separately.
	if tools := a.agentTools(&ag); strings.Contains(strings.Join(tools, ","), "run_command") || len(tools) != 4 {
		t.Fatalf("tools: %v", tools)
	}

	// Writing asks first; Deny leaves no file.
	var asked []Action
	ts.decide = func(act Action) bool { asked = append(asked, act); return false }
	tr := ts.chat(map[string]any{"agent_id": ag.ID, "message": `use: write_file {"path":"notes/plan.md","content":"# Plan\nBuy milk"}`})
	if len(asked) != 1 || !strings.Contains(asked[0].Title, "notes/plan.md") || !strings.Contains(asked[0].Detail, "Buy milk") {
		t.Fatalf("approval request: %+v", asked)
	}
	if res := toolResults(tr); len(res) != 1 || !strings.Contains(res[0], "denied") {
		t.Fatalf("denied result: %v", res)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes", "plan.md")); err == nil {
		t.Fatal("file written although denied")
	}

	// Allow writes it; reading and listing don't ask.
	asked = nil
	ts.decide = func(act Action) bool { asked = append(asked, act); return true }
	ts.chat(map[string]any{"agent_id": ag.ID, "message": `use: write_file {"path":"notes/plan.md","content":"# Plan\nBuy milk"}`})
	if b, err := os.ReadFile(filepath.Join(ws, "notes", "plan.md")); err != nil || string(b) != "# Plan\nBuy milk" {
		t.Fatalf("written file: %q %v", b, err)
	}
	asked = nil
	tr = ts.chat(map[string]any{"agent_id": ag.ID, "message": `use: read_file {"path":"notes/plan.md"}`})
	if res := toolResults(tr); len(asked) != 0 || len(res) != 1 || !strings.Contains(res[0], "Buy milk") {
		t.Fatalf("read: asked %v, result %v", asked, res)
	}
	if out := a.runTool(t.Context(), nil, &ag, toolCall("list_files", `{"path":"notes"}`)); !strings.Contains(out, "plan.md") {
		t.Fatalf("list: %q", out)
	}

	// Nothing outside the workspace: .., absolute paths and links out.
	os.Symlink(outside, filepath.Join(ws, "link"))
	for _, p := range []string{"../../" + filepath.Base(outside) + "/secret.txt", filepath.Join(outside, "secret.txt"), "link/secret.txt"} {
		out := a.runTool(t.Context(), nil, &ag, toolCall("read_file", `{"path":`+jsonString(p)+`}`))
		if strings.Contains(out, "top secret") || !strings.HasPrefix(out, "Error:") {
			t.Errorf("read %q escaped the workspace: %q", p, out)
		}
	}
	if out := a.runTool(t.Context(), nil, &ag, toolCall("write_file", `{"path":"link/new.txt","content":"x"}`)); !strings.Contains(out, "outside the workspace") {
		t.Errorf("write through a link out: %q", out)
	}

	// Terminal commands: refused until allowed, then run after approval.
	if out := a.runTool(t.Context(), nil, &ag, toolCall("run_command", `{"command":"echo hi"}`)); !strings.Contains(out, "terminal commands are off") {
		t.Fatalf("terminal off: %q", out)
	}
	if runtime.GOOS != "windows" {
		a.SaveComputerSettings(ComputerSettings{Enabled: true, Workspace: ws, Terminal: true})
		asked = nil
		tr = ts.chat(map[string]any{"agent_id": ag.ID, "message": `use: run_command {"command":"echo hello-from-agent && pwd"}`})
		res := toolResults(tr)
		if len(asked) != 1 || asked[0].Detail == "" || len(res) != 1 || !strings.Contains(res[0], "hello-from-agent") || !strings.Contains(res[0], "workspace") {
			t.Fatalf("command: asked %+v, result %v", asked, res)
		}
	}

	// An unanswered request can be answered only once and only while waiting.
	if err := a.AnswerAction("nope", true); err == nil {
		t.Error("answer to an unknown action accepted")
	}
}

func TestNoWebAndComputerTogether(t *testing.T) {
	a := newWebApp(t)
	if _, err := a.SaveAgent(Agent{Name: "Both", Tools: []string{"web_search", "write_file"}}); err == nil || !strings.Contains(err.Error(), "can't have both") {
		t.Fatalf("web + computer agent saved: %v", err)
	}
	if _, err := a.SaveAgent(Agent{Name: "Computer", Tools: []string{"write_file", "calculator"}}); err != nil {
		t.Fatal(err)
	}
	// The Computer Assistant template is valid and has no web tools.
	for _, tpl := range AgentTemplates {
		if tpl.Name == "Computer Assistant" {
			if _, err := a.SaveAgent(tpl); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("no Computer Assistant template")
}

func TestWorkspaceSettings(t *testing.T) {
	a := newWebApp(t)
	home, _ := os.UserHomeDir()
	for _, bad := range []string{"relative/folder", home, "/"} {
		if _, err := a.SaveComputerSettings(ComputerSettings{Enabled: true, Workspace: bad}); err == nil {
			t.Errorf("workspace %q accepted", bad)
		}
	}
	cs, _ := a.ComputerSettings()
	if cs.Enabled || !strings.HasSuffix(cs.Workspace, "Private AI Workspace") {
		t.Errorf("defaults: %+v", cs)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
