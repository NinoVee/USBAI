// Package rag turns user documents into searchable chunks: text extraction,
// chunking, and a hybrid vector + keyword index.
package rag

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// ErrUnsupported is returned for file types Extract cannot read.
var ErrUnsupported = errors.New("unsupported file type")

// SupportedExtensions lists the file types Extract understands.
var SupportedExtensions = []string{".pdf", ".docx", ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".html", ".htm", ".xml", ".log", ".rtf"}

// Extract returns the plain text of a document. Everything happens in-process;
// nothing is sent anywhere.
func Extract(filename string, data []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	var (
		text string
		err  error
	)
	switch ext {
	case ".pdf":
		text, err = extractPDF(data)
	case ".docx":
		text, err = extractDOCX(data)
	case ".html", ".htm":
		text = stripHTML(string(data))
	case ".rtf":
		text = stripRTF(string(data))
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".xml", ".log":
		if !utf8.Valid(data) {
			data = bytes.ToValidUTF8(data, []byte("�"))
		}
		text = string(data)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupported, ext)
	}
	if err != nil {
		return "", err
	}
	text = normalizeSpace(text)
	if strings.TrimSpace(text) == "" {
		return "", errors.New("no text found (scanned PDFs need OCR, which is not supported yet)")
	}
	return text, nil
}

func extractPDF(data []byte) (text string, err error) {
	// The PDF parser can panic on malformed files; never let that take
	// down the app.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("could not read PDF: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("could not read PDF: %w", err)
	}
	var b strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		rows, err := p.GetTextByRow()
		if err != nil {
			// Fall back to the plain text stream for this page.
			t, err2 := p.GetPlainText(nil)
			if err2 != nil {
				continue
			}
			b.WriteString(t)
			b.WriteString("\n\n")
			continue
		}
		for _, row := range rows {
			var line strings.Builder
			for _, w := range row.Content {
				line.WriteString(w.S)
			}
			b.WriteString(strings.TrimSpace(line.String()))
			b.WriteByte('\n')
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("could not read DOCX: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != "word/document.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		return docxText(io.LimitReader(rc, 200<<20))
	}
	return "", errors.New("could not read DOCX: word/document.xml missing")
}

// docxText walks WordprocessingML, keeping text runs, tabs and paragraph breaks.
func docxText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(r)
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return b.String(), nil
		}
		if err != nil {
			return "", fmt.Errorf("could not read DOCX: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteString("\n\n")
			case "tc":
				b.WriteString(" | ")
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
}

var (
	reScript = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	reBlock  = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|table|ul|ol)[^>]*>`)
	reTag    = regexp.MustCompile(`<[^>]+>`)
	reInline = regexp.MustCompile(`(?i)</?(b|i|em|strong|span|a|code|small|sup|sub|u|mark|abbr)\b[^>]*>`)
	reRTF    = regexp.MustCompile(`\\[a-z]+-?\d* ?|[{}]`)
)

func stripHTML(s string) string {
	s = reScript.ReplaceAllString(s, " ")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reInline.ReplaceAllString(s, "") // keep "to<b>day</b>" as one word
	s = reTag.ReplaceAllString(s, " ")
	return html.UnescapeString(s)
}

// stripRTF is a rough RTF-to-text conversion: good enough for search.
func stripRTF(s string) string {
	s = strings.ReplaceAll(s, `\par`, "\n")
	return reRTF.ReplaceAllString(s, "")
}

var (
	reSpaces = regexp.MustCompile(`[ \t\x{00a0}]+`)
	reBlank  = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

func normalizeSpace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = reSpaces.ReplaceAllString(s, " ")
	s = reBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
