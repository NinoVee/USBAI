package app

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/platform"
)

func TestCalculate(t *testing.T) {
	cases := map[string]string{
		"1234 * 5678 + 99":   "7006751",
		"2 ^ 3 ^ 2":          "512",
		"-(3 + 4) * 2":       "-14",
		"10 % 3":             "1",
		"sqrt(16) + abs(-2)": "6",
		"round(2/3, 2)":      "0.67",
		"max(1, 7, 3)":       "7",
		"1,200 × 12":         "14400",
		"1.5e3 / 3":          "500",
	}
	for expr, want := range cases {
		v, err := Calculate(expr)
		if err != nil || formatNumber(v) != want {
			t.Errorf("Calculate(%q) = %v, %v; want %s", expr, formatNumber(v), err, want)
		}
	}
	for _, bad := range []string{"", "1/0", "2 +", "os.exit()", "(1", strings.Repeat("(", 500) + "1"} {
		if _, err := Calculate(bad); err == nil {
			t.Errorf("Calculate(%q) should fail", bad)
		}
	}
}

// testServer runs the app against the fake llama-server with a vision-capable
// chat model, already unlocked.
type testServer struct {
	t    *testing.T
	a    *App
	base string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	root := t.TempDir()
	host := platform.Detect()
	host.Accel = nil
	rt := filepath.Join(root, "runtime", host.Slug()+"-cpu")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(rt, host.ServerBinary()), "./testdata/fakellama").CombinedOutput(); err != nil {
		t.Fatalf("build fake server: %v\n%s", err, out)
	}
	for _, f := range []string{"models/chat/vl.gguf", "models/chat/mmproj.gguf"} {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte("GGUF"), 0o644)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Models = []config.Model{{ID: "vl", Name: "VL", Role: "chat", File: "models/chat/vl.gguf", MMProj: "models/chat/mmproj.gguf", Context: 8192}}
	a := New(cfg, host, io.Discard)
	t.Cleanup(a.Stop)
	a.StartEngines()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: a.Handler(fstest.MapFS{}, ln.Addr().(*net.TCPAddr).Port)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	ts := &testServer{t: t, a: a, base: "http://" + ln.Addr().String()}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, _, ok := a.chatEngine(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chat engine not ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := ts.call("POST", "/api/unlock", map[string]any{"passphrase": "test passphrase", "create": true}, nil); code != 200 {
		t.Fatalf("unlock: %d", code)
	}
	return ts
}

func (ts *testServer) call(method, path string, body, out any) int {
	ts.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.base+path, rd)
	req.Header.Set("X-Private-AI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type turn struct {
	events map[string][]json.RawMessage
	answer string
}

func (ts *testServer) chat(body map[string]any) turn {
	ts.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.base+"/api/chat", bytes.NewReader(b))
	req.Header.Set("X-Private-AI", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	tr := turn{events: map[string][]json.RawMessage{}}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	var ev string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			ev = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := json.RawMessage(strings.TrimPrefix(line, "data: "))
			tr.events[ev] = append(tr.events[ev], data)
			if ev == "token" {
				var x struct{ Text string }
				json.Unmarshal(data, &x)
				tr.answer += x.Text
			}
		}
	}
	return tr
}

func TestAgentUsesTools(t *testing.T) {
	ts := newTestServer(t)

	var catalog struct {
		Tools     []ToolInfo
		Templates []Agent
	}
	ts.call("GET", "/api/agents/catalog", nil, &catalog)
	if len(catalog.Tools) != len(toolCatalog) || len(catalog.Templates) == 0 {
		t.Fatalf("catalog: %+v", catalog)
	}

	var ag Agent
	if code := ts.call("POST", "/api/agents", Agent{Name: "Math", Instructions: "Be exact.", Tools: []string{"calculator", "bogus"}, Knowledge: "none", Temperature: 0.2}, &ag); code != 200 || ag.ID == "" {
		t.Fatalf("create agent: %d %+v", code, ag)
	}
	if len(ag.Tools) != 1 || ag.Emoji == "" {
		t.Fatalf("agent not sanitized: %+v", ag)
	}

	tr := ts.chat(map[string]any{"agent_id": ag.ID, "message": "calc: 1234 * 5678 + 99"})
	if len(tr.events["error"]) > 0 {
		t.Fatalf("error: %s", tr.events["error"][0])
	}
	if len(tr.events["tool"]) != 1 || len(tr.events["tool_result"]) != 1 {
		t.Fatalf("tool events: %v", tr.events)
	}
	if !strings.Contains(tr.answer, "Tool said: 7006751") || !strings.Contains(tr.answer, "[agent]") {
		t.Fatalf("answer: %q", tr.answer)
	}

	// The chat remembers its agent and the tool steps.
	var meta struct {
		ChatID string `json:"chat_id"`
	}
	json.Unmarshal(tr.events["meta"][0], &meta)
	var c Chat
	ts.call("GET", "/api/chats/"+meta.ChatID, nil, &c)
	if c.AgentID != ag.ID || len(c.Messages) != 2 || len(c.Messages[1].Steps) != 1 || c.Messages[1].Steps[0].Result != "7006751" {
		t.Fatalf("stored chat: %+v", c)
	}

	// A model that keeps repeating the same call is cut off after one rerun.
	tr = ts.chat(map[string]any{"agent_id": ag.ID, "message": "loop: 6*7"})
	if len(tr.events["tool"]) != 1 || !strings.Contains(tr.answer, "Tool said: 42") {
		t.Fatalf("repeat calls: %d tool events, answer %q", len(tr.events["tool"]), tr.answer)
	}

	// Without an agent, tools are never offered.
	tr = ts.chat(map[string]any{"message": "calc: 2+2"})
	if len(tr.events["tool"]) != 0 || strings.Contains(tr.answer, "[agent]") {
		t.Fatalf("plain chat used tools: %q", tr.answer)
	}

	// Editing and deleting.
	ag.Name = "Math Tutor"
	ts.call("POST", "/api/agents", ag, nil)
	var agents []Agent
	ts.call("GET", "/api/agents", nil, &agents)
	if len(agents) != 1 || agents[0].Name != "Math Tutor" {
		t.Fatalf("agents after edit: %+v", agents)
	}
	ts.call("DELETE", "/api/agents/"+ag.ID, nil, nil)
	ts.call("GET", "/api/agents", nil, &agents)
	if len(agents) != 0 {
		t.Fatalf("agent not deleted: %+v", agents)
	}
	tr = ts.chat(map[string]any{"chat_id": meta.ChatID, "message": "hi"})
	if len(tr.events["error"]) != 1 {
		t.Fatal("chat with deleted agent should fail clearly")
	}
}

func TestImagesInChat(t *testing.T) {
	ts := newTestServer(t)
	// 1x1 PNG.
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)

	tr := ts.chat(map[string]any{"message": "", "images": []string{dataURL}})
	if len(tr.events["error"]) > 0 {
		t.Fatalf("error: %s", tr.events["error"][0])
	}
	if !strings.Contains(tr.answer, "I see 1 image(s)") {
		t.Fatalf("answer: %q", tr.answer)
	}
	var meta struct {
		ChatID string   `json:"chat_id"`
		Images []string `json:"images"`
	}
	json.Unmarshal(tr.events["meta"][0], &meta)
	if len(meta.Images) != 1 {
		t.Fatalf("meta: %+v", meta)
	}
	resp, err := http.Get(ts.base + "/api/images/" + meta.Images[0])
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, png) || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("stored image: %d bytes, %s", len(got), resp.Header.Get("Content-Type"))
	}

	// Follow-up turns mention earlier images without re-sending them.
	tr = ts.chat(map[string]any{"chat_id": meta.ChatID, "message": "and now?"})
	if !strings.Contains(tr.answer, "No documents") {
		t.Fatalf("follow-up: %q", tr.answer)
	}

	// Non-images are rejected even if labelled as images.
	tr = ts.chat(map[string]any{"message": "x", "images": []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("#!/bin/sh"))}})
	if len(tr.events["error"]) != 1 {
		t.Fatal("non-image accepted")
	}

	// Deleting the chat deletes its images.
	ts.call("DELETE", "/api/chats/"+meta.ChatID, nil, nil)
	if _, _, err := ts.a.Image(meta.Images[0]); err == nil {
		t.Fatal("image survived chat deletion")
	}
}

// TestChatModelFallback reproduces a model copied to the drive incompletely
// and one that fails to load: the app must fall back to a working model and
// say why.
func TestChatModelFallback(t *testing.T) {
	root := t.TempDir()
	host := platform.Detect()
	host.Accel = nil
	host.RAMBytes = 16 << 30
	rt := filepath.Join(root, "runtime", host.Slug()+"-cpu")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(rt, host.ServerBinary()), "./testdata/fakellama").CombinedOutput(); err != nil {
		t.Fatalf("build fake server: %v\n%s", err, out)
	}
	for _, f := range []string{"models/chat/truncated.gguf", "models/chat/broken.gguf", "models/chat/good.gguf"} {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte("GGUF"), 0o644)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Models = []config.Model{
		{ID: "truncated", Name: "Truncated", Role: "chat", File: "models/chat/truncated.gguf", Size: 2_497_281_664, MinRAMGB: 8},
		{ID: "broken", Name: "Broken", Role: "chat", File: "models/chat/broken.gguf", MinRAMGB: 8},
		{ID: "good", Name: "Good", Role: "chat", File: "models/chat/good.gguf", Size: 4, MinRAMGB: 4},
	}
	a := New(cfg, host, io.Discard)
	defer a.Stop()
	a.startChat()

	if _, m, ok := a.chatEngine(); !ok || m.ID != "good" {
		t.Fatalf("expected fallback to good model, got %q ready=%v err=%q", m.ID, ok, a.chatErr)
	}
	for _, want := range []string{"Truncated: truncated.gguf is incomplete", "copy it to the drive again", "Broken: llama_model_load: error loading model"} {
		if !strings.Contains(a.chatWarn, want) {
			t.Errorf("warning %q lacks %q", a.chatWarn, want)
		}
	}
}
