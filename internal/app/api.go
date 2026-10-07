package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/ninovee/usbai/internal/rag"
	"github.com/ninovee/usbai/internal/vault"
)

const maxUpload = 256 << 20

// Handler returns the HTTP handler serving the UI (from ui) and the API.
func (a *App) Handler(ui fs.FS, port int) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(ui))

	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("POST /api/unlock", a.handleUnlock)
	mux.HandleFunc("POST /api/lock", func(w http.ResponseWriter, r *http.Request) {
		a.lock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		a.shutdownOnce.Do(func() { close(a.Shutdown) })
	})
	mux.HandleFunc("POST /api/models/select", a.handleSelectModel)
	mux.HandleFunc("POST /api/vision/select", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"` // "" automatic, "off", or a model id
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, err)
			return
		}
		s := a.loadSettings()
		s.VisionModel = body.ID
		if err := a.saveSettings(s); err != nil {
			httpError(w, err)
			return
		}
		go a.startVision()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/cache", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, err)
			return
		}
		s := a.loadSettings()
		s.CacheModels = body.Enabled
		if err := a.saveSettings(s); err != nil {
			httpError(w, err)
			return
		}
		if body.Enabled {
			go a.cacheRunning() // cache what's already loaded
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /api/cache", func(w http.ResponseWriter, r *http.Request) {
		respondErr(w, a.ClearCache())
	})
	mux.HandleFunc("POST /api/models/retry", func(w http.ResponseWriter, r *http.Request) {
		go a.startChat()
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/chat", a.handleChat)
	mux.HandleFunc("GET /api/chats", func(w http.ResponseWriter, r *http.Request) {
		respond(w)(a.Chats())
	})
	mux.HandleFunc("GET /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := validID(r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		respond(w)(a.GetChat(r.PathValue("id")))
	})
	mux.HandleFunc("DELETE /api/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := validID(r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		respondErr(w, a.DeleteChat(r.PathValue("id")))
	})

	mux.HandleFunc("GET /api/docs", func(w http.ResponseWriter, r *http.Request) {
		respond(w)(a.Documents())
	})
	mux.HandleFunc("POST /api/docs", a.handleUpload)
	mux.HandleFunc("GET /api/docs/{id}/file", a.handleDocFile)
	mux.HandleFunc("DELETE /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := validID(r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		respondErr(w, a.DeleteDocument(r.PathValue("id")))
	})

	mux.HandleFunc("GET /api/images/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := validID(r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		data, ctype, err := a.Image(r.PathValue("id"))
		if err != nil {
			httpError(w, err)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(data)
	})

	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		respond(w)(a.Agents())
	})
	mux.HandleFunc("GET /api/agents/catalog", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"tools": ToolInfos(), "templates": AgentTemplates})
	})
	mux.HandleFunc("POST /api/agents", func(w http.ResponseWriter, r *http.Request) {
		var ag Agent
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ag); err != nil {
			httpError(w, err)
			return
		}
		respond(w)(a.SaveAgent(ag))
	})
	mux.HandleFunc("DELETE /api/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := validID(r.PathValue("id")); err != nil {
			httpError(w, err)
			return
		}
		respondErr(w, a.DeleteAgent(r.PathValue("id")))
	})

	mux.HandleFunc("GET /api/web", func(w http.ResponseWriter, r *http.Request) {
		ws, err := a.WebSettings()
		if err != nil {
			httpError(w, err)
			return
		}
		// Never send the key back; just whether one is saved.
		writeJSON(w, map[string]any{"duckduckgo": ws.DuckDuckGo, "brave": ws.Brave, "brave_key_saved": ws.BraveKey != ""})
	})
	mux.HandleFunc("POST /api/web", func(w http.ResponseWriter, r *http.Request) {
		var ws WebSettings
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&ws); err != nil {
			httpError(w, err)
			return
		}
		respondErr(w, a.SaveWebSettings(ws))
	})
	mux.HandleFunc("POST /api/web/test", func(w http.ResponseWriter, r *http.Request) {
		res, provider, err := a.WebSearch(r.Context(), "weather today")
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, map[string]any{"provider": provider, "results": len(res)})
	})

	mux.HandleFunc("GET /api/memory", func(w http.ResponseWriter, r *http.Request) {
		respond(w)(a.Memory())
	})
	mux.HandleFunc("POST /api/memory", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, err)
			return
		}
		respond(w)(a.Remember(body.Text))
	})
	mux.HandleFunc("DELETE /api/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
		respondErr(w, a.Forget(r.PathValue("id")))
	})

	return guard(mux, port)
}

// guard protects the local API from other websites open in the same
// browser: it rejects foreign Host headers (DNS rebinding) and requires a
// custom header on every mutating request, which a cross-site page cannot
// send without a CORS preflight that this server never approves.
func guard(next http.Handler, port int) http.Handler {
	allowed := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
		fmt.Sprintf("[::1]:%d", port):     true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "local access only", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Private-AI") != "1" {
			http.Error(w, "missing X-Private-AI header", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

type modelInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Present  bool   `json:"present"`
	Problem  string `json:"problem,omitempty"`
	MinRAMGB int    `json:"min_ram_gb"`
	Active   bool   `json:"active"`
	Vision   bool   `json:"vision"`
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.engMu.Lock()
	st := map[string]any{
		"chat_state":        a.chatState,
		"chat_error":        a.chatErr,
		"chat_warning":      a.chatWarn,
		"chat_loading_secs": int(time.Since(a.chatSince).Seconds()),
		"chat_load_secs":    int(a.chatLoadDur.Seconds()),
		"vision_state":      a.visionState,
		"vision_model":      a.visionModel.Name,
		"vision_error":      a.visionErr,
		"chat_model":        a.chatModel.Name,
		"chat_vision":       a.chatModel.ID != "" && a.cfg.Vision(a.chatModel),
		"chat_runtime":      a.chatRuntime,
		"embed_state":       a.embedState,
		"embed_model":       a.embedModel.Name,
		"context_size":      a.chatModel.Context,
		"active_model":      a.chatModel.ID,
		"supported_ext":     rag.SupportedExtensions,
	}
	active := a.chatModel.ID
	// Images work if the chat model sees them, or the image reader is up.
	st["images_ok"] = (a.chatState == StateReady && a.chatModel.ID != "" && a.cfg.Vision(a.chatModel)) ||
		(a.visionState == StateReady && a.vision != nil && a.vision.Alive())
	st["vision_choice"] = a.loadSettings().VisionModel
	st["cache_on"] = a.loadSettings().CacheModels
	a.engMu.Unlock()

	var models []modelInfo
	for _, m := range a.cfg.ModelsByRole("chat") {
		mi := modelInfo{ID: m.ID, Name: m.Name, Present: a.cfg.Present(m), MinRAMGB: m.MinRAMGB, Active: m.ID == active, Vision: a.cfg.Vision(m)}
		if mi.Present {
			if err := a.cfg.Check(m); err != nil {
				mi.Present, mi.Problem = false, err.Error()
			}
		}
		models = append(models, mi)
	}
	st["models"] = models
	st["cache_names"], st["cache_bytes"] = a.CacheStatus()
	st["cache_dir"] = a.cacheDir()
	st["host"] = a.host
	st["version"] = a.Version
	st["initialized"] = a.Initialized()
	a.dataMu.RLock()
	st["locked"] = a.vault == nil
	a.dataMu.RUnlock()
	writeJSON(w, st)
}

func (a *App) handleUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Passphrase string `json:"passphrase"`
		Create     bool   `json:"create"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, err)
		return
	}
	if body.Create && a.Initialized() {
		httpError(w, errors.New("this drive already has a vault"))
		return
	}
	if !body.Create && !a.Initialized() {
		httpError(w, errors.New("no vault yet — create one first"))
		return
	}
	if err := a.unlock(body.Passphrase, body.Create); err != nil {
		if errors.Is(err, vault.ErrWrongPassphrase) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleSelectModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, err)
		return
	}
	found := false
	for _, m := range a.cfg.ModelsByRole("chat") {
		if m.ID == body.ID && a.cfg.Check(m) == nil {
			found = true
		}
	}
	if !found {
		httpError(w, fmt.Errorf("model %q is not on this drive", body.ID))
		return
	}
	s := a.loadSettings()
	s.ChatModel = body.ID
	if err := a.saveSettings(s); err != nil {
		httpError(w, err)
		return
	}
	go a.startChat()
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		httpError(w, err)
		return
	}
	type result struct {
		Name  string   `json:"name"`
		Doc   *DocMeta `json:"doc,omitempty"`
		Error string   `json:"error,omitempty"`
	}
	var results []result
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpError(w, err)
			return
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		if part.FormName() != "file" || name == "" || name == "." {
			part.Close()
			continue
		}
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			httpError(w, err)
			return
		}
		meta, err := a.AddDocument(name, data)
		if err != nil {
			if errors.Is(err, errLocked) {
				httpError(w, err)
				return
			}
			results = append(results, result{Name: name, Error: err.Error()})
			continue
		}
		results = append(results, result{Name: name, Doc: &meta})
	}
	writeJSON(w, results)
}

func (a *App) handleDocFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := validID(id); err != nil {
		httpError(w, err)
		return
	}
	meta, data, err := a.DocumentFile(id)
	if err != nil {
		httpError(w, err)
		return
	}
	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(meta.Name)))
	// Serve active content as plain text so an uploaded HTML file cannot run
	// script in this origin.
	if ctype == "" || strings.Contains(ctype, "html") || strings.Contains(ctype, "xml") || strings.Contains(ctype, "svg") {
		ctype = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": meta.Name}))
	w.Write(data)
}

// sseEvents streams chat progress as server-sent events.
type sseEvents struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s sseEvents) send(event string, v any) error {
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

func (s sseEvents) Meta(chatID string, sources []Source, images []string) error {
	return s.send("meta", map[string]any{"chat_id": chatID, "sources": sources, "images": images})
}
func (s sseEvents) ToolCall(t ToolStep) error   { return s.send("tool", t) }
func (s sseEvents) ToolResult(t ToolStep) error { return s.send("tool_result", t) }
func (s sseEvents) Thinking() error             { return s.send("thinking", map[string]any{}) }
func (s sseEvents) Token(text string) error     { return s.send("token", map[string]string{"text": text}) }

func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, (maxImages*maxImageBytes*4/3)+(1<<20))).Decode(&req); err != nil {
		httpError(w, err)
		return
	}
	f, ok := w.(http.Flusher)
	if !ok {
		httpError(w, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	ev := sseEvents{w: w, f: f}
	if err := a.Chat(req, ev); err != nil {
		ev.send("error", map[string]string{"error": err.Error()})
		return
	}
	ev.send("done", map[string]any{})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, errLocked):
		code = http.StatusLocked
	case errors.Is(err, vault.ErrNotFound):
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

func respond(w http.ResponseWriter) func(any, error) {
	return func(v any, err error) {
		if err != nil {
			httpError(w, err)
			return
		}
		writeJSON(w, v)
	}
}

func respondErr(w http.ResponseWriter, err error) {
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
