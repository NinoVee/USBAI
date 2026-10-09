package app

import (
	"strings"

	"github.com/ninovee/usbai/internal/config"
)

// Context window: how many tokens the chat model can hold at once (the
// conversation, document excerpts, tool results and the answer). A bigger
// window needs more memory for the model's working cache, so "auto" picks
// the biggest one that fits next to the system, the browser and an image
// reader.

// contextChoices are the sizes offered in Settings.
var contextChoices = []int{8192, 16384, 32768}

// kvBytesPerToken is a generous estimate of the working cache each token
// of context needs for the models on the drive (Qwen3 4B/8B need about
// 144 KB; gpt-oss and Gemma 3 need less thanks to sliding windows).
const kvBytesPerToken = 160 << 10

// contextHeadroom is the memory "auto" keeps free beside the chat model and
// its cache: the system, the browser and an image reader.
const contextHeadroom = 8 << 30

// kvBytes estimates the working cache for n tokens of context.
func kvBytes(n int) int64 { return int64(n) * kvBytesPerToken }

// chatContext returns the context window to load m with.
func (a *App) chatContext(m config.Model) int {
	floor := m.Context
	if floor <= 0 {
		floor = 4096
	}
	want := a.loadSettings().ContextSize
	if want > 0 {
		return want // the user's choice, even if it may not fit
	}
	ram := int64(a.host.RAMBytes)
	if ram <= 0 || m.Size <= 0 {
		return floor
	}
	best := floor
	for _, n := range contextChoices {
		if n > best && m.Size+m.MMProjSize+kvBytes(n)+contextHeadroom <= ram {
			best = n
		}
	}
	return best
}

// isContextOverflow reports whether the model server refused a request for
// being longer than the context window.
func isContextOverflow(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "exceed_context_size") || strings.Contains(err.Error(), "exceeds the available context size"))
}
