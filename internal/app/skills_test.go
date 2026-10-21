package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/platform"
)

// tarGz builds a GitHub-style archive: every file under one top folder.
func tarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: "pack-abc123/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestSkillPack installs a skill pack from a source archive (shared by all
// computers, no Node.js needed) and reads it with the skill tools.
func TestSkillPack(t *testing.T) {
	archive := tarGz(map[string]string{
		"ads/SKILL.md":                         "---\nname: ads\ndescription: \"Run paid ads work.\"\n---\n# Claude Ads\nLoad references/thinking-framework.md first.",
		"ads/references/thinking-framework.md": "Think in evidence.",
		"skills/ads-google/SKILL.md":           "---\nname: ads-google\ndescription: Audit Google Ads.\n---\nRead ads/references/google-audit.md.",
		"ads/references/google-audit.md":       "Check search terms.",
		"tests/fixture/SKILL.md":               "---\nname: not-a-skill\n---\n",
		"scripts/tool.sh":                      "echo hi",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer srv.Close()

	cfg := config.Default()
	cfg.Root = t.TempDir()
	cfg.Apps = []config.App{{ID: "claude-ads", Name: "Claude Ads", Kind: "skills", Shared: true, Source: srv.URL + "/pack.tar.gz"}}
	a := New(cfg, platform.Detect(), os.Stderr)
	defer a.Stop()
	if a.appToolAvailable("read_skill") {
		t.Fatal("skill tools offered before the pack is installed")
	}
	if err := a.apps.Install("claude-ads"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); !a.apps.Installed("claude-ads"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			log, _ := a.apps.Log("claude-ads")
			t.Fatalf("not installed:\n%s", log)
		}
	}
	if _, err := os.Stat(cfg.Root + "/apps/claude-ads/shared/ads/SKILL.md"); err != nil {
		t.Fatalf("pack not in the shared folder: %v", err)
	}
	if !a.appToolAvailable("read_skill") {
		t.Fatal("skill tools not offered after install")
	}
	arg := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	list, err := a.skillTool("list_skills", arg(nil))
	if err != nil || !strings.Contains(list, "- ads: Run paid ads work.") || !strings.Contains(list, "- ads-google: Audit Google Ads.") || strings.Contains(list, "not-a-skill") {
		t.Fatalf("list_skills: %v\n%s", err, list)
	}
	body, err := a.skillTool("read_skill", arg(map[string]string{"name": "ads-google"}))
	if err != nil || !strings.Contains(body, "Read ads/references/google-audit.md") || !strings.Contains(body, "skills/ads-google") {
		t.Fatalf("read_skill: %v\n%s", err, body)
	}
	if out, err := a.skillTool("read_skill_file", arg(map[string]string{"path": "ads/references/google-audit.md"})); err != nil || out != "Check search terms." {
		t.Fatalf("read_skill_file: %v %q", err, out)
	}
	// A path relative to the skill's own folder is found by its ending.
	if out, err := a.skillTool("read_skill_file", arg(map[string]string{"path": "references/thinking-framework.md"})); err != nil || out != "Think in evidence." {
		t.Fatalf("relative read_skill_file: %v %q", err, out)
	}
	for _, bad := range []string{"../../config.json", "/etc/passwd", "scripts/tool.sh", "ads/../../../x.md"} {
		if out, err := a.skillTool("read_skill_file", arg(map[string]string{"path": bad})); err == nil {
			t.Errorf("read %s: %q", bad, out)
		}
	}
	var hasTemplate bool
	for _, tpl := range a.agentTemplates() {
		hasTemplate = hasTemplate || tpl.Name == "Ads Auditor"
	}
	if !hasTemplate {
		t.Error("Ads Auditor template missing")
	}
}

// TestAppRunEnv checks which AI an app is given: the Claude key first, then
// the OpenAI key, and nothing for apps that don't use AI.
func TestAppRunEnv(t *testing.T) {
	ts := newTestServerModels(t, []config.Model{
		{ID: "vl", Name: "VL", Role: "chat", File: "models/chat/vl.gguf", Context: 8192},
		{ID: "gpt", Name: "GPT", Role: "chat", Provider: "openai", RemoteModel: "gpt-x", Context: 100000},
	})
	ts.a.cfg.Apps = []config.App{{ID: "vt", AI: "langchain"}, {ID: "plain"}}
	if env := ts.a.appRunEnv("plain"); env != nil {
		t.Errorf("app without AI got %v", env)
	}
	// No keys: the local model on the drive.
	if env := ts.a.appRunEnv("vt"); env["LANGCHAIN_PROVIDER"] != "openai" || !strings.HasSuffix(env["OPENAI_BASE_URL"], "/v1") || env["LANGCHAIN_MODEL_NAME"] != "vl" {
		t.Errorf("local: %v", env)
	}
	ts.call("POST", "/api/cloud", map[string]string{"openai_key": "sk-o"}, nil)
	if env := ts.a.appRunEnv("vt"); env["OPENAI_API_KEY"] != "sk-o" || env["LANGCHAIN_MODEL_NAME"] != "gpt-x" || env["OPENAI_BASE_URL"] != "" {
		t.Errorf("openai: %v", env)
	}
	ts.call("POST", "/api/cloud", map[string]string{"anthropic_key": "sk-a"}, nil)
	if env := ts.a.appRunEnv("vt"); env["LANGCHAIN_PROVIDER"] != "anthropic" || env["ANTHROPIC_API_KEY"] != "sk-a" {
		t.Errorf("anthropic: %v", env)
	}
}
