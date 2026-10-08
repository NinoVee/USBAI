package app

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestCleanTranscript(t *testing.T) {
	for in, want := range map[string]string{
		"language English<asr_text>Ask not what your country can do.": "Ask not what your country can do.",
		"language None<asr_text>":                                     "",
		"  plain text  ":                                              "plain text",
		"language English<asr_text>Hi.</asr_text>":                    "Hi.",
	} {
		if got := cleanTranscript(in); got != want {
			t.Errorf("cleanTranscript(%q) = %q, want %q", in, got, want)
		}
	}
}

func silentWAV(samples int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+samples*2))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)})
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(samples*2))
	b.Write(make([]byte, samples*2))
	return b.Bytes()
}

func TestSpeechToText(t *testing.T) {
	root := t.TempDir()
	host := platform.Detect()
	host.Accel = nil
	rt := filepath.Join(root, "runtime", host.Slug()+"-cpu")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(rt, host.ServerBinary()), "./testdata/fakellama").CombinedOutput(); err != nil {
		t.Fatalf("build fake server: %v\n%s", err, out)
	}
	for _, f := range []string{"models/chat/m.gguf", "models/speech/asr.gguf", "models/speech/asr-mmproj.gguf"} {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte("GGUF"), 0o644)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Models = []config.Model{
		{ID: "m", Name: "M", Role: "chat", File: "models/chat/m.gguf"},
		{ID: "asr", Name: "ASR", Role: "speech", File: "models/speech/asr.gguf", MMProj: "models/speech/asr-mmproj.gguf"},
	}
	a := New(cfg, host, io.Discard)
	defer a.Stop()
	if err := a.unlock("test passphrase", true); err != nil {
		t.Fatal(err)
	}
	a.StartEngines()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := a.speechEngine(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("speech engine not ready: %s", a.speechState)
		}
		time.Sleep(50 * time.Millisecond)
	}

	text, err := a.Transcribe(silentWAV(16000))
	if err != nil || text != "Hello from the microphone." {
		t.Fatalf("transcribe: %q %v", text, err)
	}
	if _, err := a.Transcribe([]byte("not audio at all, definitely not a wav file here")); err == nil || !strings.Contains(err.Error(), "WAV") {
		t.Fatalf("non-WAV accepted: %v", err)
	}
	if _, err := a.Transcribe(make([]byte, maxSpeechBytes+1)); err == nil {
		t.Fatal("oversized recording accepted")
	}
}

func TestVoiceFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"models/voice/kokoro/onnx/model_quantized.onnx": "ONNX",
		"models/voice/kokoro/voices/af_heart.bin":       "VOICE",
		"models/voice/kokoro/ort/ort.wasm":              "WASM",
		"models/voice/kokoro/secret.txt":                "not listed",
		"data/vault/key":                                "never",
	}
	for f, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte(body), 0o644)
	}
	cfg := config.Default()
	cfg.Root = root
	cfg.Models = []config.Model{{ID: "kokoro", Role: "voice", File: "models/voice/kokoro/onnx/model_quantized.onnx", Size: 4,
		Extra: []config.ExtraFile{{File: "models/voice/kokoro/voices/af_heart.bin", Size: 5}, {File: "models/voice/kokoro/ort/ort.wasm", Size: 4}}}}
	a := New(cfg, platform.Detect(), io.Discard)
	defer a.Stop()
	if ids := a.voiceIDs(); len(ids) != 1 || ids[0] != "af_heart" {
		t.Fatalf("voices: %v", ids)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = a.Handler(fstest.MapFS{}, srv.Listener.Addr().(*net.TCPAddr).Port)
	srv.Start()
	defer srv.Close()
	get := func(p string) (int, string, http.Header) {
		req, _ := http.NewRequest("GET", srv.URL+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	for p, want := range map[string]string{
		"/tts/onnx-community/Kokoro-82M-v1.0-ONNX/resolve/main/onnx/model_quantized.onnx": "ONNX",
		"/tts/onnx-community/Kokoro-82M-v1.0-ONNX/resolve/main/voices/af_heart.bin":       "VOICE",
		"/tts/ort/ort.wasm": "WASM",
	} {
		code, body, h := get(p)
		if code != 200 || body != want {
			t.Errorf("%s: %d %q", p, code, body)
		}
		if h.Get("Cross-Origin-Embedder-Policy") != "require-corp" {
			t.Errorf("%s: no cross-origin isolation headers", p)
		}
	}
	for _, p := range []string{"/tts/secret.txt", "/tts/x/y/resolve/main/secret.txt", "/tts/../../data/vault/key", "/tts/ort/..%2F..%2F..%2F..%2Fdata%2Fvault%2Fkey"} {
		if code, body, _ := get(p); code == 200 {
			t.Errorf("%s served: %q", p, body)
		}
	}
}
