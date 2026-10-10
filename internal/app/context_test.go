package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ninovee/usbai/internal/config"
)

func TestAutoContext(t *testing.T) {
	a := newWebApp(t)
	const gb = 1 << 30
	vl4b := config.Model{Size: 2_500_000_000, MMProjSize: 800_000_000, Context: 8192}
	gptoss := config.Model{Size: 12_109_566_624, Context: 16384}
	for _, c := range []struct {
		ram  uint64
		m    config.Model
		want int
	}{
		{24 * gb, vl4b, 32768},   // the user's MacBook Air
		{24 * gb, gptoss, 16384}, // leaves room for the image reader
		{16 * gb, vl4b, 16384},
		{8 * gb, vl4b, 8192}, // never below the model's own setting
		{0, vl4b, 8192},      // unknown memory
	} {
		a.host.RAMBytes = c.ram
		if got := a.chatContext(c.m); got != c.want {
			t.Errorf("%d GB, %d bytes: context %d, want %d", c.ram/gb, c.m.Size, got, c.want)
		}
	}
	// The user's choice wins.
	s := a.loadSettings()
	s.ContextSize = 32768
	a.saveSettings(s)
	a.host.RAMBytes = 8 * gb
	if got := a.chatContext(vl4b); got != 32768 {
		t.Errorf("chosen 32K: got %d", got)
	}
}

// A turn whose tool results and history don't fit the context window is
// trimmed to fit instead of failing with "exceeds the available context
// size".
func TestContextOverflow(t *testing.T) {
	ts := newTestServer(t)
	a := ts.a
	ws := filepath.Join(t.TempDir(), "workspace")
	os.MkdirAll(ws, 0o755)
	os.WriteFile(filepath.Join(ws, "big.txt"), []byte(strings.Repeat("pressure points relax the body. ", 400)), 0o644)
	a.SaveComputerSettings(ComputerSettings{Enabled: true, Workspace: ws})
	var ag Agent
	ts.call("POST", "/api/agents", Agent{Name: "Reader", Knowledge: "none", Tools: []string{"read_file"}}, &ag)

	// A small window: the model's server is restarted with 2048 tokens.
	s := a.loadSettings()
	s.ContextSize = 2048
	a.saveSettings(s)
	a.startChat()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, m, ok := a.chatEngine(); ok && m.Context == 2048 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("chat engine not restarted")
		}
		time.Sleep(50 * time.Millisecond)
	}

	long := strings.Repeat("Tell me about relaxation. ", 120)
	tr := ts.chat(map[string]any{"agent_id": ag.ID, "message": long})
	chatID := ""
	if len(tr.events["error"]) > 0 {
		t.Fatalf("first turn: %s", tr.events["error"][0])
	}
	for _, raw := range tr.events["meta"] {
		var m struct {
			ChatID string `json:"chat_id"`
		}
		json.Unmarshal(raw, &m)
		chatID = m.ChatID
	}
	for i := 0; i < 3; i++ {
		ts.chat(map[string]any{"agent_id": ag.ID, "chat_id": chatID, "message": long})
	}
	tr = ts.chat(map[string]any{"agent_id": ag.ID, "chat_id": chatID, "message": `use: read_file {"path":"big.txt"}`})
	if len(tr.events["error"]) > 0 {
		t.Fatalf("overflowing turn failed: %s", tr.events["error"][0])
	}
	if res := toolResults(tr); len(res) != 1 || !strings.Contains(res[0], "pressure points") {
		t.Fatalf("tool result: %v", res)
	}
	if tr.answer == "" {
		t.Fatal("no answer")
	}
}

// A model with a fixed Greeting opens its first reply in a chat with that
// exact text, and does not repeat it on later turns.
func TestFixedGreeting(t *testing.T) {
	ts := newTestServerModels(t, []config.Model{{
		ID: "gang", Name: "GANG", Role: "chat", File: "models/chat/vl.gguf",
		Context: 8192, Greeting: "Wat up homie? Wat it do?", Avoid: []string{"no cap"},
	}})
	tr := ts.chat(map[string]any{"message": "hello"})
	if !strings.HasPrefix(tr.answer, "Wat up homie? Wat it do?\n\n") {
		t.Fatalf("first reply did not open with the greeting: %q", tr.answer)
	}
	// When the model writes its own greeting too, it is stripped so the
	// catchphrase appears exactly once.
	tr = ts.chat(map[string]any{"message": "greet-too please"})
	if n := strings.Count(tr.answer, "Wat up homie?"); n != 1 {
		t.Fatalf("greeting should appear once, got %d: %q", n, tr.answer)
	}
	if strings.Contains(strings.ToLower(tr.answer), "no cap") {
		t.Fatalf("banned phrase not stripped: %q", tr.answer)
	}
	if !strings.Contains(tr.answer, "kickin' it") {
		t.Fatalf("model answer lost: %q", tr.answer)
	}
	// The saved reply has duplicate sign-offs collapsed.
	chats, _ := ts.a.Chats()
	saved, _ := ts.a.GetChat(chats[0].ID)
	last := saved.Messages[len(saved.Messages)-1].Content
	if n := strings.Count(last, "That's real spit"); n != 1 {
		t.Fatalf("duplicate sign-off not collapsed in saved reply, got %d: %q", n, last)
	}
	var meta struct {
		ChatID string `json:"chat_id"`
	}
	json.Unmarshal(tr.events["meta"][0], &meta)

	tr = ts.chat(map[string]any{"chat_id": meta.ChatID, "message": "and again"})
	if strings.Contains(tr.answer, "Wat up homie?") {
		t.Fatalf("greeting repeated on a later turn: %q", tr.answer)
	}
}

func TestAvoidAndTidy(t *testing.T) {
	re := avoidRegexp([]string{"no cap"})
	if got := re.ReplaceAllString("I'm chillin', no cap. For real", ""); got != "I'm chillin'. For real" {
		t.Errorf("strip: %q", got)
	}
	if got := tidyReply("Line one.\n\n\nThat's real spit.\nThat's real spit."); got != "Line one.\n\nThat's real spit." {
		t.Errorf("tidy: %q", got)
	}
}
