package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ninovee/usbai/internal/rag"
)

func (ts *testServer) upload(fields map[string]string, name string, data []byte) []map[string]any {
	ts.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, k := range []string{"ocr_text", "ocr_model"} {
		if v, ok := fields[k]; ok {
			mw.WriteField(k, v)
		}
	}
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", ts.base+"/api/docs", &body)
	req.Header.Set("X-Private-AI", "1")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestScannedPDF(t *testing.T) {
	ts := newTestServer(t)
	pdf, err := os.ReadFile("testdata/scanned.pdf")
	if err != nil {
		t.Fatal(err)
	}

	// A scan has no text layer: the upload asks the browser for OCR.
	res := ts.upload(nil, "memo.pdf", pdf)
	if len(res) != 1 || res[0]["needs_ocr"] != true || res[0]["doc"] != nil {
		t.Fatalf("first upload: %v", res)
	}
	// A text file with no text is not a scan.
	if res := ts.upload(nil, "empty.txt", []byte("  \n")); res[0]["needs_ocr"] != nil {
		t.Fatalf("empty txt: %v", res)
	}

	// The vision model reads a page; code fences are removed.
	page := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
	var ocr struct{ Text, Model string }
	if code := ts.call("POST", "/api/ocr", map[string]string{"image": page}, &ocr); code != 200 {
		t.Fatalf("ocr: %d", code)
	}
	if ocr.Text != "MEMO 7: the budget is $342,500." || ocr.Model != "VL" {
		t.Fatalf("ocr: %+v", ocr)
	}
	if code := ts.call("POST", "/api/ocr", map[string]string{"image": "data:text/plain;base64,aGk="}, nil); code == 200 {
		t.Fatal("non-image accepted")
	}

	// Uploading again with the text stores the original file and indexes the text.
	res = ts.upload(map[string]string{"ocr_text": "--- Page 1 ---\n" + ocr.Text, "ocr_model": ocr.Model}, "memo.pdf", pdf)
	doc, _ := res[0]["doc"].(map[string]any)
	if doc == nil || doc["ocr"] != "VL" {
		t.Fatalf("second upload: %v", res)
	}
	id := doc["id"].(string)
	resp, err := http.Get(ts.base + "/api/docs/" + id + "/file")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	got.ReadFrom(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got.Bytes(), pdf) {
		t.Fatal("original PDF not kept")
	}
	tr := ts.chat(map[string]any{"message": "what is the budget?", "use_docs": true, "doc_ids": []string{id}})
	if !strings.Contains(tr.answer, "342,500") {
		t.Fatalf("answer did not use the scanned text: %q", tr.answer)
	}
}

func TestScannedPDFDetection(t *testing.T) {
	for file, want := range map[string]error{"testdata/image-only.pdf": rag.ErrScanned, "testdata/scanned.pdf": rag.ErrUnreadablePDF} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rag.Extract("x.pdf", data); !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", file, err, want)
		}
	}
}
