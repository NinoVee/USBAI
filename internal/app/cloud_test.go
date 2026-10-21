package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ninovee/usbai/internal/cloud"
	"github.com/ninovee/usbai/internal/config"
)

// TestCloudModels drives the app with a local model and two cloud models
// whose APIs are fakes: keys are saved but never shown, cloud models run
// only when chosen, and each reply comes from its provider with its key.
func TestCloudModels(t *testing.T) {
	var mu sync.Mutex
	var claudeKey, claudeSystem, openaiAuth, openaiModel string
	claude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			System []struct{ Text string } `json:"system"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		claudeKey = r.Header.Get("X-Api-Key")
		if len(body.System) > 0 {
			claudeSystem = body.System[0].Text
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(name, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
		ev("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello from Claude."}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	defer claude.Close()
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		openaiAuth = r.Header.Get("Authorization")
		openaiModel, _ = body["model"].(string)
		_, hasTemp := body["temperature"]
		mu.Unlock()
		if hasTemp {
			http.Error(w, `{"error":{"message":"Unsupported value: 'temperature'"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello from ChatGPT.\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer openai.Close()
	oldC, oldO := cloud.ClaudeBaseURL, cloud.OpenAIBaseURL
	cloud.ClaudeBaseURL, cloud.OpenAIBaseURL = claude.URL, openai.URL
	defer func() { cloud.ClaudeBaseURL, cloud.OpenAIBaseURL = oldC, oldO }()

	ts := newTestServerModels(t, []config.Model{
		{ID: "claude", Name: "Claude", Role: "chat", Provider: "anthropic", RemoteModel: "claude-opus-5-5", Context: 100000},
		{ID: "vl", Name: "VL", Role: "chat", File: "models/chat/vl.gguf", Context: 8192},
		{ID: "gpt", Name: "GPT", Role: "chat", Provider: "openai", RemoteModel: "gpt-5.5", Context: 100000},
	})
	// A cloud model listed first is still never picked on its own.
	if _, m, _ := ts.a.chatEngine(); m.ID != "vl" {
		t.Fatalf("started with %q, want the local model", m.ID)
	}

	waitModel := func(id string) {
		t.Helper()
		if code := ts.call("POST", "/api/models/select", map[string]string{"id": id}, nil); code != 200 {
			t.Fatalf("select %s: %d", id, code)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, m, ok := ts.a.chatEngine(); ok && m.ID == id {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("model %s not active", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Without a key, the cloud model says where to add one.
	waitModel("claude")
	tr := ts.chat(map[string]any{"message": "hi"})
	var errs string
	for _, e := range tr.events["error"] {
		errs += string(e)
	}
	if !strings.Contains(errs, "Claude API key") {
		t.Fatalf("want a missing-key error, got %s / %q", errs, tr.answer)
	}

	if code := ts.call("POST", "/api/cloud", map[string]string{"anthropic_key": "sk-ant-test", "openai_key": "sk-openai-test"}, nil); code != 200 {
		t.Fatalf("save keys: %d", code)
	}
	if code := ts.call("POST", "/api/cloud", map[string]string{"openai_model": "gpt-test"}, nil); code != 200 {
		t.Fatalf("save model: %d", code)
	}
	var cs map[string]any
	ts.call("GET", "/api/cloud", nil, &cs)
	if b, _ := json.Marshal(cs); strings.Contains(string(b), "sk-") || cs["anthropic_key_saved"] != true || cs["openai_key_saved"] != true || cs["openai_model"] != "gpt-test" {
		t.Fatalf("GET /api/cloud = %s", b)
	}

	tr = ts.chat(map[string]any{"message": "hi"})
	if tr.answer != "Hello from Claude." {
		t.Fatalf("Claude answer = %q (errors %q)", tr.answer, tr.events["error"])
	}
	mu.Lock()
	if claudeKey != "sk-ant-test" || !strings.Contains(claudeSystem, "is sent to that provider") {
		t.Errorf("Claude got key %q, system %q", claudeKey, claudeSystem)
	}
	mu.Unlock()

	waitModel("gpt")
	tr = ts.chat(map[string]any{"message": "hi"})
	if tr.answer != "Hello from ChatGPT." {
		t.Fatalf("ChatGPT answer = %q (errors %q)", tr.answer, tr.events["error"])
	}
	mu.Lock()
	if openaiAuth != "Bearer sk-openai-test" || openaiModel != "gpt-test" {
		t.Errorf("OpenAI got auth %q, model %q", openaiAuth, openaiModel)
	}
	mu.Unlock()

	var st map[string]any
	ts.call("GET", "/api/status", nil, &st)
	if st["cloud_models"] != true || st["chat_runtime"] != "cloud" {
		t.Errorf("status cloud_models=%v runtime=%v", st["cloud_models"], st["chat_runtime"])
	}
}

// testClient keeps the session cookie the app gives on unlock, like a
// browser does.
var testClient = func() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}()
