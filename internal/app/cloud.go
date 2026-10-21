package app

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ninovee/usbai/internal/cloud"
	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/llama"
	"github.com/ninovee/usbai/internal/vault"
)

// Cloud models (Claude and ChatGPT) are config entries with a provider and
// no files. They run only when the user picks one and has saved that
// provider's API key; the key stays encrypted in the vault and is sent
// only to that provider.

// CloudSettings is stored encrypted in the vault (it holds the API keys).
type CloudSettings struct {
	AnthropicKey string `json:"anthropic_key,omitempty"`
	OpenAIKey    string `json:"openai_key,omitempty"`
	// OpenAIModel overrides the ChatGPT model id (OpenAI renames models
	// often); empty uses the config entry's.
	OpenAIModel string `json:"openai_model,omitempty"`
}

// providerNames are shown to the user.
var providerNames = map[string]string{"anthropic": "Claude (Anthropic)", "openai": "ChatGPT (OpenAI)"}

// CloudSettings returns the saved cloud settings.
func (a *App) CloudSettings() (CloudSettings, error) {
	var cs CloudSettings
	v, err := a.unlocked()
	if err != nil {
		return cs, err
	}
	if err := v.GetJSON("cloud_settings", &cs); err != nil && !errors.Is(err, vault.ErrNotFound) {
		return cs, err
	}
	return cs, nil
}

// CloudUpdate changes the cloud settings. An empty key keeps the saved
// one and "-" removes it; a nil OpenAIModel keeps the saved model name.
type CloudUpdate struct {
	AnthropicKey string  `json:"anthropic_key"`
	OpenAIKey    string  `json:"openai_key"`
	OpenAIModel  *string `json:"openai_model"`
}

// SaveCloudSettings applies an update to the saved settings.
func (a *App) SaveCloudSettings(up CloudUpdate) error {
	v, err := a.unlocked()
	if err != nil {
		return err
	}
	old, err := a.CloudSettings()
	if err != nil {
		return err
	}
	keep := func(nw, prev string) string {
		switch nw = strings.TrimSpace(nw); nw {
		case "":
			return prev
		case "-":
			return ""
		}
		return nw
	}
	cs := CloudSettings{
		AnthropicKey: keep(up.AnthropicKey, old.AnthropicKey),
		OpenAIKey:    keep(up.OpenAIKey, old.OpenAIKey),
		OpenAIModel:  old.OpenAIModel,
	}
	if up.OpenAIModel != nil {
		cs.OpenAIModel = strings.TrimSpace(*up.OpenAIModel)
	}
	return v.PutJSON("cloud_settings", cs)
}

// hasCloudModels reports whether config.json lists any cloud model.
func (a *App) hasCloudModels() bool {
	for _, m := range a.models() {
		if m.Cloud() {
			return true
		}
	}
	return false
}

// chatStream runs one chat completion on the local engine at url, or on
// the cloud provider of a cloud model. It returns the tool calls and, for
// Claude, the assistant turn in Claude's own form (see llama.Message.Raw).
func (a *App) chatStream(ctx context.Context, url string, m config.Model, msgs []llama.Message, opts llama.ChatOptions, onDelta func(llama.Delta) error) ([]llama.ToolCall, any, error) {
	if !m.Cloud() {
		calls, err := llama.ChatStream(ctx, url, msgs, opts, onDelta)
		return calls, nil, err
	}
	cs, err := a.CloudSettings()
	if err != nil {
		return nil, nil, err
	}
	switch m.Provider {
	case "anthropic":
		if cs.AnthropicKey == "" {
			return nil, nil, errors.New("add your Claude API key in Settings → Cloud AI to use " + m.Name)
		}
		return cloud.ClaudeStream(ctx, cs.AnthropicKey, m.RemoteModel, msgs, opts, onDelta)
	case "openai":
		if cs.OpenAIKey == "" {
			return nil, nil, errors.New("add your OpenAI API key in Settings → Cloud AI to use " + m.Name)
		}
		opts.APIKey, opts.Model = cs.OpenAIKey, m.RemoteModel
		if cs.OpenAIModel != "" {
			opts.Model = cs.OpenAIModel
		}
		calls, err := llama.ChatStream(ctx, cloud.OpenAIBaseURL, msgs, opts, onDelta)
		return calls, nil, openAIError(err)
	}
	return nil, nil, errors.New("unknown cloud provider " + m.Provider + " in config.json")
}

// openAIError turns an OpenAI failure into a message the user can act on.
func openAIError(err error) error {
	var se *llama.StatusError
	if !errors.As(err, &se) {
		return err
	}
	switch {
	case se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden:
		return errors.New("the OpenAI API key was rejected — check it in Settings → Cloud AI")
	case se.Code == http.StatusTooManyRequests:
		return errors.New("OpenAI's rate limit or your credit was reached — wait a moment, or check your OpenAI billing")
	case se.Code == http.StatusNotFound || strings.Contains(se.Msg, "model_not_found"):
		return errors.New("OpenAI doesn't offer that model to your key — change the ChatGPT model name in Settings → Cloud AI")
	}
	return errors.New(strings.Replace(se.Msg, "model server:", "OpenAI:", 1))
}

// TestCloud sends a tiny request to check a provider's key.
func (a *App) TestCloud(ctx context.Context, provider string) (string, error) {
	var m config.Model
	for _, c := range a.models() {
		if c.Provider == provider {
			m = c
			break
		}
	}
	if m.ID == "" {
		return "", errors.New("no " + provider + " model in config.json")
	}
	var b strings.Builder
	msgs := []llama.Message{{Role: "user", Content: "Reply with just the word OK."}}
	_, _, err := a.chatStream(ctx, "", m, msgs, llama.ChatOptions{MaxTokens: 2000}, func(d llama.Delta) error {
		b.WriteString(d.Content)
		return nil
	})
	if err != nil {
		return "", err
	}
	return providerNames[provider] + " answered: " + strings.TrimSpace(b.String()), nil
}

// cloudNote tells a cloud model where it runs, so it never tells the user
// their chat stays on the computer.
func cloudNote(m config.Model) string {
	return "\n\nNote: in this chat you are running as " + m.Name + " on " + providerNames[m.Provider] +
		"'s servers, using the user's own API key. Everything in this chat — messages, images, document excerpts and the remembered " +
		"notes above — is sent to that provider. Never tell the user this chat stays on their computer."
}
