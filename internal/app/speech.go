package app

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/llama"
)

// Talk-to-text: a small speech recognition model (role "speech", e.g.
// Qwen3-ASR) runs as its own llama.cpp engine. The browser records the
// microphone and sends 16 kHz WAV audio, which never leaves this computer.

const maxSpeechBytes = 8 << 20 // about 4 minutes of 16 kHz mono 16-bit audio

func (a *App) startSpeech() {
	var m *config.Model
	for _, c := range a.cfg.ModelsByRole("speech") {
		if a.cfg.Check(c) == nil && a.cfg.Vision(c) { // needs its audio encoder too
			m = &c
			break
		}
	}
	a.engMu.Lock()
	if m == nil {
		a.speechState = StateNoModel
		a.engMu.Unlock()
		return
	}
	a.speechModel = *m
	a.engMu.Unlock()

	srv, _, err := a.startServer(a.ctx, *m, false)

	a.engMu.Lock()
	defer a.engMu.Unlock()
	if a.ctx.Err() != nil {
		srv.Stop()
		return
	}
	if err != nil {
		a.speechState = StateError
		a.logf("Speech recognition failed to start; voice input is off: %v", err)
		return
	}
	a.speech, a.speechState = srv, StateReady
	a.logf("Speech recognition ready: %s", m.Name)
}

func (a *App) speechEngine() (string, bool) {
	a.engMu.Lock()
	defer a.engMu.Unlock()
	if a.speech == nil || !a.speech.Alive() {
		return "", false
	}
	return a.speech.BaseURL, true
}

// Transcribe turns a WAV recording into text.
func (a *App) Transcribe(wav []byte) (string, error) {
	if _, err := a.unlocked(); err != nil {
		return "", err
	}
	if len(wav) > maxSpeechBytes {
		return "", errors.New("recording is too long: keep it under 4 minutes")
	}
	if len(wav) < 44 || !bytes.Equal(wav[:4], []byte("RIFF")) || !bytes.Equal(wav[8:12], []byte("WAVE")) {
		return "", errors.New("audio must be a WAV recording")
	}
	url, ok := a.speechEngine()
	if !ok {
		a.engMu.Lock()
		state := a.speechState
		a.engMu.Unlock()
		switch state {
		case StateStarting:
			return "", errors.New("the voice model is still loading — try again in a moment")
		case StateNoModel:
			return "", errors.New("no speech recognition model on this drive (see README: Voice)")
		}
		return "", errors.New("the voice model failed to start")
	}
	msgs := []llama.Message{{Role: "user", Content: []llama.Part{
		{Type: "input_audio", InputAudio: &llama.InputAudio{Data: base64.StdEncoding.EncodeToString(wav), Format: "wav"}},
	}}}
	var b strings.Builder
	_, err := llama.ChatStream(a.ctx, url, msgs, llama.ChatOptions{Temperature: 0, MaxTokens: 1500}, func(d llama.Delta) error {
		b.WriteString(d.Content)
		return nil
	})
	if err != nil {
		return "", err
	}
	return cleanTranscript(b.String()), nil
}

// cleanTranscript drops the "language English<asr_text>" header Qwen3-ASR
// writes before the text; "language None" means no speech was heard.
func cleanTranscript(s string) string {
	s = strings.TrimSpace(stripThink(s))
	if i := strings.Index(s, "<asr_text>"); i >= 0 {
		if strings.Contains(s[:i], "None") && strings.TrimSpace(s[i+10:]) == "" {
			return ""
		}
		s = s[i+len("<asr_text>"):]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "</asr_text>"))
}
