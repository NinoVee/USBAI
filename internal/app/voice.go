package app

import (
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ninovee/usbai/internal/config"
)

// Natural voices: a speech-synthesis model (role "voice", Kokoro) runs in
// the browser with the bundled kokoro-js and ONNX Runtime Web. Its files
// live on the drive and are served here, so nothing is fetched from the
// internet. Files are laid out as in the model's Hugging Face repository
// under the model's folder (the parent of File's onnx/ folder).

// voiceModel returns the first complete voice model on the drive.
func (a *App) voiceModel() (config.Model, bool) {
	for _, m := range a.cfg.ModelsByRole("voice") {
		if a.cfg.Check(m) == nil {
			return m, true
		}
	}
	return config.Model{}, false
}

// voiceIDs lists the voices of the voice model, such as "af_heart".
func (a *App) voiceIDs() []string {
	m, ok := a.voiceModel()
	if !ok {
		return nil
	}
	var ids []string
	for _, x := range m.Extra {
		if dir, file := path.Split(x.File); strings.HasSuffix(dir, "/voices/") && reVoiceFile.MatchString(file) {
			ids = append(ids, strings.TrimSuffix(file, ".bin"))
		}
	}
	sort.Strings(ids)
	return ids
}

// Named voices: a (US) or b (UK), f or m, then a name, e.g. "am_michael".
var reVoiceFile = regexp.MustCompile(`^[a-z][fm]_[a-z]+\.bin$`)

// "/tts/<org>/<repo>/resolve/<revision>/<file>" is how transformers.js asks
// for model files; "/tts/ort/<file>" is the ONNX runtime.
var reHubPath = regexp.MustCompile(`^[^/]+/[^/]+/resolve/[^/]+/`)

func (a *App) handleVoiceFile(w http.ResponseWriter, r *http.Request) {
	m, ok := a.voiceModel()
	if !ok {
		http.NotFound(w, r)
		return
	}
	rel := reHubPath.ReplaceAllString(r.PathValue("path"), "")
	root := path.Dir(path.Dir(m.File))
	want := path.Join(root, rel)
	// Only the model's own files, never anything else on the drive.
	allowed := want == m.File
	for _, x := range m.Extra {
		allowed = allowed || want == x.File
	}
	if !allowed || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(a.cfg.Path(want))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(want) {
	case ".wasm":
		w.Header().Set("Content-Type", "application/wasm")
	case ".mjs", ".js":
		w.Header().Set("Content-Type", "text/javascript")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeContent(w, r, "", st.ModTime(), f)
}
