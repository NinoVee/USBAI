package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
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

// TestEndToEnd builds a fake llama-server into a temporary drive and drives
// the real app through unlock, upload, retrieval-augmented chat, memory,
// lock and re-unlock.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a helper binary")
	}
	root := t.TempDir()
	host := platform.Detect()
	host.Accel = nil // force the CPU runtime folder
	rt := filepath.Join(root, "runtime", host.Slug()+"-cpu")
	build := exec.Command("go", "build", "-o", filepath.Join(rt, host.ServerBinary()), "./testdata/fakellama")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake server: %v\n%s", err, out)
	}
	for _, f := range []string{"models/chat/small.gguf", "models/embed/embed.gguf"} {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte("GGUF"), 0o644)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Models = []config.Model{
		{ID: "big", Name: "Big", Role: "chat", File: "models/chat/missing.gguf", MinRAMGB: 1},
		{ID: "small", Name: "Small", Role: "chat", File: "models/chat/small.gguf", MinRAMGB: 1, Context: 4096},
		{ID: "emb", Name: "Emb", Role: "embedding", File: "models/embed/embed.gguf", Context: 512, QueryPrefix: "q: ", DocumentPrefix: "d: "},
	}

	a := New(cfg, host, io.Discard)
	defer a.Stop()
	a.StartEngines()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: a.Handler(fstest.MapFS{"index.html": {Data: []byte("ui")}}, port)}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	call := func(method, path string, body any, out any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("X-Private-AI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}

	// Wait for both engines.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var st map[string]any
		call("GET", "/api/status", nil, &st)
		if st["chat_state"] == StateReady && st["embed_state"] == StateReady {
			if st["active_model"] != "small" {
				t.Fatalf("picked %v, want the model that is present", st["active_model"])
			}
			break
		}
		if st["chat_state"] == StateError || time.Now().After(deadline) {
			t.Fatalf("engines not ready: %v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Mutating requests without the header are refused (CSRF guard).
	resp, _ := http.Post(base+"/api/lock", "application/json", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing header: got %d", resp.StatusCode)
	}

	if code := call("GET", "/api/docs", nil, nil); code != http.StatusLocked {
		t.Fatalf("docs before unlock: %d", code)
	}
	if code := call("POST", "/api/unlock", map[string]any{"passphrase": "test passphrase", "create": true}, nil); code != 200 {
		t.Fatalf("create vault: %d", code)
	}

	// Upload a document.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "contract.txt")
	fw.Write([]byte("SERVICE AGREEMENT\n\nPayment terms: the client pays invoices within 30 days.\n\nTermination requires 60 days notice."))
	mw.Close()
	req, _ := http.NewRequest("POST", base+"/api/docs", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Private-AI", "1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var up []struct {
		Doc   *DocMeta
		Error string
	}
	json.NewDecoder(resp.Body).Decode(&up)
	resp.Body.Close()
	if len(up) != 1 || up[0].Doc == nil || !up[0].Doc.Embedded {
		t.Fatalf("upload: %+v", up)
	}
	docID := up[0].Doc.ID

	if code := call("POST", "/api/memory", map[string]string{"text": "My name is Sam."}, nil); code != 200 {
		t.Fatalf("remember: %d", code)
	}

	chat := func(body map[string]any) (events map[string][]string) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+"/api/chat", bytes.NewReader(b))
		req.Header.Set("X-Private-AI", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		events = map[string][]string{}
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				ev = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				events[ev] = append(events[ev], strings.TrimPrefix(line, "data: "))
			}
		}
		return events
	}

	ev := chat(map[string]any{"message": "When are invoices paid?", "use_docs": true})
	if len(ev["error"]) > 0 || len(ev["done"]) != 1 {
		t.Fatalf("chat events: %v", ev)
	}
	var meta struct {
		ChatID  string   `json:"chat_id"`
		Sources []Source `json:"sources"`
	}
	json.Unmarshal([]byte(ev["meta"][0]), &meta)
	if len(meta.Sources) == 0 || meta.Sources[0].DocName != "contract.txt" {
		t.Fatalf("expected contract source, got %+v", meta.Sources)
	}
	var answer strings.Builder
	for _, tok := range ev["token"] {
		var x struct{ Text string }
		json.Unmarshal([]byte(tok), &x)
		answer.WriteString(x.Text)
	}
	if !strings.Contains(answer.String(), "30 days") {
		t.Fatalf("answer did not include retrieved context: %q", answer.String())
	}
	if len(ev["thinking"]) != 1 {
		t.Fatalf("expected one thinking event, got %v", ev["thinking"])
	}

	// Follow-up in the same chat, focused on the document (full-text mode).
	ev = chat(map[string]any{"chat_id": meta.ChatID, "message": "Summarize it", "doc_ids": []string{docID}})
	var c Chat
	call("GET", "/api/chats/"+meta.ChatID, nil, &c)
	if len(c.Messages) != 4 {
		t.Fatalf("chat has %d messages, want 4", len(c.Messages))
	}
	if !strings.Contains(c.Messages[3].Content, "Got 4 messages") {
		t.Fatalf("history not sent: %q", c.Messages[3].Content)
	}
	if !strings.Contains(a.systemPrompt(nil), "My name is Sam.") {
		t.Fatal("memory missing from system prompt")
	}

	// Lock, then unlock with the wrong and right passphrase.
	call("POST", "/api/lock", nil, nil)
	if code := call("GET", "/api/chats", nil, nil); code != http.StatusLocked {
		t.Fatalf("chats after lock: %d", code)
	}
	if code := call("POST", "/api/unlock", map[string]any{"passphrase": "nope nope nope"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("wrong passphrase: %d", code)
	}
	call("POST", "/api/unlock", map[string]any{"passphrase": "test passphrase"}, nil)
	var docs []DocMeta
	call("GET", "/api/docs", nil, &docs)
	if len(docs) != 1 || docs[0].Name != "contract.txt" {
		t.Fatalf("docs after re-unlock: %+v", docs)
	}
	if res := a.index.Search("termination notice", nil, 1, nil); len(res) != 1 {
		t.Fatal("index not restored after unlock")
	}

	// Nothing readable on disk.
	filepath.Walk(filepath.Join(root, "data"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte("invoices")) || bytes.Contains(b, []byte("Sam")) {
				t.Errorf("plaintext found in %s", p)
			}
		}
		return nil
	})
}
