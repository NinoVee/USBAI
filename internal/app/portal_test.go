package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ninovee/usbai/internal/config"
)

// TestPortal shows a Conway account through a portal: the key is saved in
// the vault and never shown, and only https links and terminals pass.
func TestPortal(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Authorization")
		if gotKey != "conway-key" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/credits/balance":
			w.Write([]byte(`{"balance_cents": 1234}`))
		case "/v1/sandboxes":
			w.Write([]byte(`{"sandboxes":[{"id":"sb-1","status":"running","region":"us-east","vcpu":2,"memory_mb":4096,"terminal_url":"https://term.conway.tech/sb-1"},
				{"sandbox_id":"sb-2","terminal_url":"javascript:alert(1)"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := conwayAPI
	conwayAPI = srv.URL
	defer func() { conwayAPI = old }()

	ts := newTestServer(t)
	ts.a.cfg.Apps = append(ts.a.cfg.Apps, config.App{ID: "automaton", Name: "Automaton", Kind: "portal", Status: "conway",
		Links: []config.AppLink{{Label: "Dashboard", URL: "https://app.conway.tech"}, {Label: "Bad", URL: "javascript:alert(1)"}}})

	info, err := ts.a.Portal(context.Background(), "automaton")
	if err != nil || info.KeySaved || info.Status != nil || len(info.Links) != 1 {
		t.Fatalf("before key: %+v %v", info, err)
	}
	if code := ts.call("POST", "/api/apps/automaton/key", map[string]string{"key": "wrong"}, nil); code != 200 {
		t.Fatalf("save key: %d", code)
	}
	if info, _ := ts.a.Portal(context.Background(), "automaton"); !strings.Contains(info.Error, "rejected the API key") {
		t.Errorf("wrong key: %+v", info)
	}
	ts.call("POST", "/api/apps/automaton/key", map[string]string{"key": "conway-key"}, nil)
	var got PortalInfo
	ts.call("GET", "/api/apps/automaton/portal", nil, &got)
	if !got.KeySaved || got.Status == nil || got.Status.CreditsUSD != 12.34 || len(got.Status.Machines) != 2 {
		t.Fatalf("status: %+v", got)
	}
	m1, m2 := got.Status.Machines[0], got.Status.Machines[1]
	if m1.ID != "sb-1" || m1.TerminalURL != "https://term.conway.tech/sb-1" || m1.MemoryMB != 4096 {
		t.Errorf("machine 1: %+v", m1)
	}
	if m2.ID != "sb-2" || m2.TerminalURL != "" || m2.Status != "unknown" {
		t.Errorf("machine 2 (script terminal URL must be dropped): %+v", m2)
	}
	if code := ts.call("POST", "/api/apps/nope/key", map[string]string{"key": "x"}, nil); code == 200 {
		t.Error("saved a key for an app that isn't a portal")
	}
}
