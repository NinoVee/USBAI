package app

import (
	"errors"
	"strings"

	"github.com/ninovee/usbai/internal/config"
	"github.com/ninovee/usbai/internal/llama"
)

// Scanned PDFs have no text layer. The browser renders each page to an
// image (with the bundled pdf.js) and OCRPage has a vision model read it;
// the text is then stored with the original file (AddScannedDocument).

const ocrMaxTokens = 3000 // a dense page is ~1000 tokens; stops runaway loops

const ocrPrompt = "You are an OCR engine. Transcribe all text on this scanned page exactly as written, in reading order. " +
	"Keep the paragraphs, headings and line breaks. Write tables as Markdown tables. " +
	"Write [illegible] for words you cannot read. Note stamps, signatures, handwriting and pictures briefly in [brackets]. " +
	"Do not correct, summarize or translate. Output only the transcription, with no introduction or comments. " +
	"If the page is blank, output [blank page]."

// ocrEngine picks the model that reads page images: the chat model when it
// can see, otherwise the image reader.
func (a *App) ocrEngine() (string, config.Model, error) {
	url, model, ready := a.chatEngine()
	if ready && a.cfg.Vision(model) {
		return url, model, nil
	}
	if rurl, reader, ok := a.visionEngine(); ok {
		return rurl, reader, nil
	}
	if !ready {
		return "", config.Model{}, errors.New("the AI model is still loading — try again in a moment")
	}
	return "", config.Model{}, errors.New("can't read scanned pages: " + a.noVisionReason(model))
}

// OCRPage transcribes one page image (a data: URL) and returns the text and
// the name of the model that read it.
func (a *App) OCRPage(dataURL string) (string, string, error) {
	if _, err := a.unlocked(); err != nil {
		return "", "", err
	}
	img, err := decodeDataURL(dataURL)
	if err != nil {
		return "", "", err
	}
	url, model, err := a.ocrEngine()
	if err != nil {
		return "", "", err
	}
	msgs := []llama.Message{
		{Role: "system", Content: ocrPrompt},
		{Role: "user", Content: []llama.Part{
			{Type: "image_url", ImageURL: &llama.ImageURL{URL: img.dataURL}},
			{Type: "text", Text: "Transcribe this page."},
		}},
	}
	var b strings.Builder
	_, err = llama.ChatStream(a.ctx, url, msgs, llama.ChatOptions{Temperature: 0, MaxTokens: ocrMaxTokens}, func(d llama.Delta) error {
		b.WriteString(d.Content)
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return cleanOCR(b.String()), model.Name, nil
}

// cleanOCR removes thinking and the code fences some models wrap text in.
func cleanOCR(s string) string {
	s = strings.TrimSpace(stripThink(s))
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}
