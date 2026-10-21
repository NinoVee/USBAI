package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/platform"
)

// TestBrowserTools drives the browser tools against a fake Camofox: each
// chat keeps its own tab, refs are clicked, and pages on this computer or
// the home network are refused.
func TestBrowserTools(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	tabs := 0
	page := "https://example.com/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && r.URL.Path == "/tabs":
			tabs++
			page, _ = body["url"].(string)
			json.NewEncoder(w).Encode(map[string]any{"tabId": "tab" + string(rune('0'+tabs)), "navigationOk": true})
		case strings.HasSuffix(r.URL.Path, "/navigate"):
			page, _ = body["url"].(string)
			w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/click"):
			if body["ref"] == "e9" {
				page = "http://192.168.1.1/admin" // a link into the home network
			} else {
				page = "https://example.com/about"
			}
			w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/back"):
			page = "https://example.com/"
			w.Write([]byte(`{"ok":true}`))
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			json.NewEncoder(w).Encode(map[string]any{"url": page, "snapshot": "- heading \"Example\"\n- link \"About\" [e1]"})
		default:
			http.Error(w, `{"error":"Tab not found"}`, 404)
		}
	}))
	defer srv.Close()
	testAppEndpoint = func(context.Context, string) (string, string, error) { return srv.URL, "k", nil }
	defer func() { testAppEndpoint = nil }()

	a := New(config.Default(), platform.Detect(), os.Stderr)
	defer a.Stop()
	ctx := context.WithValue(context.Background(), chatIDKey{}, "chat1")
	args := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	out, err := a.browserTool(ctx, "browser_open", args(map[string]string{"url": "example.com"}))
	if err != nil || !strings.Contains(out, "Page: https://example.com") || !strings.Contains(out, "[e1]") || !strings.Contains(out, "untrusted") {
		t.Fatalf("open: %v\n%s", err, out)
	}
	if out, err = a.browserTool(ctx, "browser_click", args(map[string]string{"ref": "e1"})); err != nil || !strings.Contains(out, "example.com/about") {
		t.Fatalf("click: %v\n%s", err, out)
	}
	// The same chat reuses its tab.
	a.browserTool(ctx, "browser_open", args(map[string]string{"url": "https://example.com/x"}))
	if tabs != 1 {
		t.Errorf("tabs opened = %d, want 1", tabs)
	}
	// A click into the home network is undone and refused.
	if _, err = a.browserTool(ctx, "browser_click", args(map[string]string{"ref": "e9"})); err == nil || !strings.Contains(err.Error(), "may not open") {
		t.Fatalf("home-network click: %v", err)
	}
	mu.Lock()
	last := calls[len(calls)-1]
	mu.Unlock()
	if !strings.HasSuffix(last, "/back") {
		t.Errorf("expected the browser to go back, calls: %v", calls)
	}
	for _, u := range []string{"http://localhost:8740/api/memory", "http://127.0.0.1/", "http://10.0.0.5/", "file:///etc/passwd", "http://printer.local/"} {
		if _, err := a.browserTool(ctx, "browser_open", args(map[string]string{"url": u})); err == nil {
			t.Errorf("opened %s", u)
		}
	}
	// Another chat gets its own tab; reading before opening explains.
	other := context.WithValue(context.Background(), chatIDKey{}, "chat2")
	if _, err := a.browserTool(other, "browser_read", args(nil)); err == nil || !strings.Contains(err.Error(), "browser_open first") {
		t.Errorf("read without a page: %v", err)
	}
}

// TestAppsLive runs the real apps from a Pro drive that has them installed
// for this computer (set GANG_PRO_DRIVE to its folder). It browses a local
// test page with Camofox and renders a video with HyperFrames.
func TestAppsLive(t *testing.T) {
	root := os.Getenv("GANG_PRO_DRIVE")
	if root == "" {
		t.Skip("set GANG_PRO_DRIVE to a Pro drive with the apps installed")
	}
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	host := platform.Detect()
	a := New(cfg, host, os.Stderr)
	defer a.Stop()
	allowPrivateIP = true
	defer func() { allowPrivateIP = false }()

	site := t.TempDir()
	os.WriteFile(filepath.Join(site, "index.html"), []byte(`<h1>Kettle shop</h1><p>The red kettle costs $42.</p><a href="about.html">About us</a>`), 0o644)
	os.WriteFile(filepath.Join(site, "about.html"), []byte(`<h1>About</h1><p>Selling kettles since 1999.</p>`), 0o644)
	web := httptest.NewServer(http.FileServer(http.Dir(site)))
	defer web.Close()
	ctx := context.WithValue(context.Background(), chatIDKey{}, "live")
	args := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	out, err := a.browserTool(ctx, "browser_open", args(map[string]string{"url": web.URL}))
	if err != nil || !strings.Contains(out, "$42") {
		t.Fatalf("open: %v\n%s", err, out)
	}
	ref := strings.TrimSuffix(strings.SplitN(strings.SplitN(out, "About us\" [", 2)[1], "]", 2)[0], "]")
	if out, err = a.browserTool(ctx, "browser_click", args(map[string]string{"ref": ref})); err != nil || !strings.Contains(out, "since 1999") {
		t.Fatalf("click %s: %v\n%s", ref, err, out)
	}

	ws := t.TempDir()
	html := `<!doctype html><html><head><script src="https://cdn.jsdelivr.net/npm/gsap@3/dist/gsap.min.js"></script>
<style>html,body{margin:0;width:640px;height:360px;overflow:hidden;background:#123}#t{color:#fff;font:48px sans-serif;margin:140px 0 0 40px}</style></head>
<body><div id="root" data-composition-id="main" data-start="0" data-duration="2" data-width="640" data-height="360">
<h1 id="t" class="clip" data-start="0" data-duration="2" data-track-index="0">Hello</h1></div>
<script>const tl=gsap.timeline({paused:true});tl.fromTo("#t",{opacity:0},{opacity:1,duration:1},0);window.__timelines["main"]=tl;</script></body></html>`
	approve := func(string, string) (bool, string) { return true, "" }
	start := time.Now()
	res := a.makeVideo(context.Background(), ComputerSettings{Enabled: true, Workspace: ws}, args(map[string]string{"name": "hello test", "html": html}), approve)
	if !strings.HasPrefix(res, "Rendered Videos/hello-test.mp4") {
		t.Fatalf("make_video: %s", res)
	}
	probe := filepath.Join(a.apps.Dir("hyperframes"), "node_modules", "@ffprobe-installer", npmPlatform(host.Slug()), "ffprobe")
	dur, err := exec.Command(probe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", filepath.Join(ws, "Videos", "hello-test.mp4")).Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(dur)), "2") {
		t.Fatalf("video duration %q: %v", dur, err)
	}
	t.Logf("rendered a 2 s video in %s: %s", time.Since(start).Round(time.Second), res)
}

func npmPlatform(slug string) string {
	return strings.NewReplacer("macos", "darwin", "windows", "win32").Replace(slug)
}
