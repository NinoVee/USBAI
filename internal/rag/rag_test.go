package rag

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestChunkCoversTextWithOverlap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("This is sentence number ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(". ")
		if i%10 == 9 {
			b.WriteString("\n\n")
		}
	}
	text := b.String()
	chunks := Chunk(text, 500, 80)
	if len(chunks) < 5 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > 500 {
			t.Errorf("chunk %d has %d runes", i, n)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(text), chunks[len(chunks)-1][len(chunks[len(chunks)-1])-20:]) {
		t.Error("last chunk does not reach end of text")
	}
}

func TestChunkShort(t *testing.T) {
	if got := Chunk("  hello  ", 100, 10); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := Chunk("   ", 100, 10); got != nil {
		t.Fatalf("got %q", got)
	}
}

func TestSearchKeywordAndVector(t *testing.T) {
	ix := NewIndex()
	ix.Add([]Piece{
		{DocID: "contract", Seq: 0, Text: "Payment terms: invoices are due within 30 days of receipt.", Vec: []float32{1, 0, 0}},
		{DocID: "contract", Seq: 1, Text: "Either party may terminate with 60 days written notice.", Vec: []float32{0, 1, 0}},
		{DocID: "recipe", Seq: 0, Text: "Bake the bread at 220 degrees for 30 minutes.", Vec: []float32{0, 0, 1}},
	})

	res := ix.Search("when are invoices due", nil, 2, nil)
	if len(res) == 0 || res[0].DocID != "contract" || res[0].Seq != 0 {
		t.Fatalf("keyword search: %+v", res)
	}

	// Vector-only match: no shared keywords, but the embedding points at the termination clause.
	res = ix.Search("ending the agreement", []float32{0.1, 0.9, 0}, 1, nil)
	if len(res) != 1 || res[0].Seq != 1 {
		t.Fatalf("vector search: %+v", res)
	}

	res = ix.Search("30", nil, 5, map[string]bool{"recipe": true})
	if len(res) != 1 || res[0].DocID != "recipe" {
		t.Fatalf("filtered search: %+v", res)
	}

	ix.RemoveDoc("contract")
	if ix.Len() != 1 || len(ix.Search("invoices", nil, 5, nil)) != 0 {
		t.Fatal("RemoveDoc left pieces behind")
	}
}

func TestExtractDOCX(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte(`<?xml version="1.0"?><w:document xmlns:w="x"><w:body>
		<w:p><w:r><w:t>Payment is due</w:t></w:r><w:r><w:t xml:space="preserve"> monthly.</w:t></w:r></w:p>
		<w:p><w:r><w:t>Second paragraph.</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	got, err := Extract("a.docx", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got != "Payment is due monthly.\n\nSecond paragraph." {
		t.Fatalf("got %q", got)
	}
}

func TestExtractHTMLAndUnsupported(t *testing.T) {
	got, err := Extract("a.html", []byte("<html><style>x{}</style><p>Hello &amp; welcome</p><p>Bye</p></html>"))
	if err != nil || !strings.Contains(got, "Hello & welcome") || !strings.Contains(got, "Bye") {
		t.Fatalf("got %q, %v", got, err)
	}
	if strings.Contains(got, "x{}") {
		t.Fatalf("style leaked: %q", got)
	}
	if _, err := Extract("a.exe", []byte("MZ")); err == nil {
		t.Fatal("expected unsupported error")
	}
}
