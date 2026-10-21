package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ninovee/usbai/internal/config"
)

// TestModelManager searches a fake Hugging Face, adds a model with an image
// projector, downloads it with checksums, and deletes models: an added
// one loses its entry, a listed one keeps it, the active one is refused and
// a personality's base needs force.
func TestModelManager(t *testing.T) {
	weights := []byte(strings.Repeat("GGUF-weights-", 1000))
	proj := []byte("GGUF-projector")
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	var hfSrv *httptest.Server
	hfSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			if r.URL.Query().Get("filter") != "gguf" || r.URL.Query().Get("search") != "tiny" {
				t.Errorf("search query: %s", r.URL.RawQuery)
			}
			w.Write([]byte(`[{"id":"acme/Tiny-GGUF","downloads":1200,"likes":7,"gated":false},{"id":"acme/Locked-GGUF","downloads":5,"likes":0,"gated":"manual"}]`))
		case r.URL.Path == "/api/models/acme/Tiny-GGUF/tree/main":
			json.NewEncoder(w).Encode([]map[string]any{
				{"type": "file", "path": "README.md", "size": 10},
				{"type": "file", "path": "tiny-Q4_K_M.gguf", "size": 99, "lfs": map[string]any{"oid": sum(weights), "size": len(weights)}},
				{"type": "file", "path": "big/tiny-F16-00002-of-00003.gguf", "size": 5},
				{"type": "file", "path": "mmproj-tiny-f16.gguf", "size": 99, "lfs": map[string]any{"oid": sum(proj), "size": len(proj)}},
			})
		case r.URL.Path == "/acme/Tiny-GGUF/resolve/main/tiny-Q4_K_M.gguf":
			w.Write(weights)
		case r.URL.Path == "/acme/Tiny-GGUF/resolve/main/mmproj-tiny-f16.gguf":
			w.Write(proj)
		case r.URL.Path == "/listed.gguf":
			w.Write(weights)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hfSrv.Close()
	old := hfBase
	hfBase = hfSrv.URL
	defer func() { hfBase = old }()

	ts := newTestServerModels(t, []config.Model{
		{ID: "vl", Name: "VL", Role: "chat", File: "models/chat/vl.gguf", Context: 8192},
		{ID: "listed", Name: "Listed", Role: "chat", File: "models/chat/listed.gguf", URL: hfSrv.URL + "/listed.gguf", Size: int64(len(weights)), SHA256: sum(weights)},
		{ID: "gang", Name: "GANG", Role: "chat", Base: "listed", Persona: "x"},
	})
	root := ts.a.cfg.Root
	os.WriteFile(filepath.Join(root, config.FileName), []byte(`{"listen_port": 9999, "models": [{"id":"vl"},{"id":"listed"},{"id":"gang","base":"listed"}]}`), 0o644)
	waitDone := func(id string) Download {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			d, ok := ts.a.Downloads()[id]
			if ok && d.State != "downloading" {
				return d
			}
			if time.Now().After(deadline) {
				t.Fatalf("download of %s did not finish: %+v", id, d)
			}
		}
	}

	var found []HFModel
	ts.call("GET", "/api/hf/search?q=tiny", nil, &found)
	if len(found) != 2 || found[0].ID != "acme/Tiny-GGUF" || found[0].Gated || !found[1].Gated {
		t.Fatalf("search: %+v", found)
	}
	var files HFFiles
	ts.call("GET", "/api/hf/files?repo=acme/Tiny-GGUF", nil, &files)
	if len(files.Models) != 1 || files.Models[0].Path != "tiny-Q4_K_M.gguf" || files.Models[0].Size != int64(len(weights)) || len(files.MMProj) != 1 {
		t.Fatalf("files (split parts and non-GGUF files must be left out): %+v", files)
	}
	if code := ts.call("GET", "/api/hf/files?repo=../../etc", nil, nil); code == 200 {
		t.Error("accepted a bad repo name")
	}
	if code := ts.call("POST", "/api/hf/add", map[string]string{"repo": "acme/Tiny-GGUF", "file": "../evil.gguf"}, nil); code == 200 {
		t.Error("added a file that isn't in the repository")
	}

	var added config.Model
	if code := ts.call("POST", "/api/hf/add", map[string]string{"repo": "acme/Tiny-GGUF", "file": "tiny-Q4_K_M.gguf", "mmproj": "mmproj-tiny-f16.gguf"}, &added); code != 200 {
		t.Fatalf("add: %d", code)
	}
	if added.ID != "hf-tiny-gguf-tiny-q4-k-m" || added.Added != "huggingface" || added.File != "models/chat/hf/acme__Tiny-GGUF/tiny-Q4_K_M.gguf" || added.MMProj == "" {
		t.Errorf("added entry: %+v", added)
	}
	if d := waitDone(added.ID); d.State != "done" || d.Done != int64(len(weights)+len(proj)) {
		t.Fatalf("download: %+v", d)
	}
	if got, _ := os.ReadFile(filepath.Join(root, added.File)); string(got) != string(weights) {
		t.Fatal("weights not on the drive")
	}
	m, _ := ts.a.findModel(added.ID)
	if !ts.a.cfg.Vision(m) || ts.a.cfg.Check(m) != nil {
		t.Errorf("added model not usable: vision=%v check=%v", ts.a.cfg.Vision(m), ts.a.cfg.Check(m))
	}
	// The entry is saved in the drive's config.json, other settings kept.
	b, _ := os.ReadFile(filepath.Join(root, config.FileName))
	if !strings.Contains(string(b), `"listen_port": 9999`) || !strings.Contains(string(b), added.ID) {
		t.Errorf("config.json after add:\n%s", b)
	}

	// The listed model downloads from its own address.
	if code := ts.call("POST", "/api/models/listed/download", nil, nil); code != 200 {
		t.Fatalf("download listed: %d", code)
	}
	waitDone("listed")
	var st struct{ Models []modelInfo }
	ts.call("GET", "/api/status", nil, &st)
	for _, mi := range st.Models {
		if mi.ID == "listed" && (!mi.Deletable || mi.CanDownload || len(mi.UsedBy) != 1) {
			t.Errorf("listed in status: %+v", mi)
		}
		if mi.ID == "vl" && !mi.Active {
			t.Errorf("vl should be active: %+v", mi)
		}
	}

	if code := ts.call("DELETE", "/api/models/vl", nil, nil); code == 200 {
		t.Error("deleted the active model")
	}
	if code := ts.call("DELETE", "/api/models/listed", nil, nil); code == 200 {
		t.Error("deleted GANG's base without force")
	}
	if code := ts.call("DELETE", "/api/models/listed?force=1", nil, nil); code != 200 {
		t.Fatalf("delete listed: %d", code)
	}
	if _, ok := ts.a.findModel("listed"); !ok {
		t.Error("a listed model's entry must stay")
	}
	var del struct{ Freed int64 }
	if code := ts.call("DELETE", "/api/models/"+added.ID, nil, &del); code != 200 || del.Freed != int64(len(weights)+len(proj)) {
		t.Fatalf("delete added: %d freed %d", code, del.Freed)
	}
	if _, ok := ts.a.findModel(added.ID); ok {
		t.Error("an added model's entry must go")
	}
	if _, err := os.Stat(filepath.Join(root, added.File)); !os.IsNotExist(err) {
		t.Error("added model's file still on the drive")
	}
	b, _ = os.ReadFile(filepath.Join(root, config.FileName))
	if strings.Contains(string(b), added.ID) || !strings.Contains(string(b), `"listed"`) {
		t.Errorf("config.json after delete:\n%s", b)
	}
}
