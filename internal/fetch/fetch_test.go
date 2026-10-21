package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDownloadResumeAndVerify resumes from a .part file with a Range
// request, checks the SHA-256, and drops a file whose checksum is wrong.
func TestDownloadResumeAndVerify(t *testing.T) {
	body := []byte(strings.Repeat("0123456789", 500))
	h := sha256.Sum256(body)
	sum := hex.EncodeToString(h[:])
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "m.gguf", time.Time{}, strings.NewReader(string(body)))
	}))
	defer srv.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	os.WriteFile(path+".part", body[:1234], 0o644) // an interrupted download
	var last int64
	if err := Download(context.Background(), srv.URL, path, sum, func(done, total int64) { last = done }); err != nil {
		t.Fatal(err)
	}
	if ranges[0] != "bytes=1234-" || last != int64(len(body)) {
		t.Errorf("ranges %v, last progress %d", ranges, last)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Error("resumed file differs")
	}
	bad := filepath.Join(dir, "bad.gguf")
	if err := Download(context.Background(), srv.URL, bad, strings.Repeat("0", 64), nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Error("file with a bad checksum was kept")
	}
}
