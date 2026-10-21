// Package fetch downloads large files (AI models) to the drive: it resumes
// an interrupted download from its .part file, retries dropped
// connections, and checks the SHA-256 before the file takes its name.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Progress is told the bytes downloaded so far and the total (0 when
// unknown).
type Progress func(done, total int64)

// StatusError is a download refused by the server.
type StatusError struct {
	URL  string
	Code int
}

func (e *StatusError) Error() string {
	switch e.Code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("%s: access denied (%d) — the file may need a login or a license accepted on its website", e.URL, e.Code)
	case http.StatusNotFound:
		return fmt.Sprintf("%s: not found", e.URL)
	}
	return fmt.Sprintf("GET %s: HTTP %d", e.URL, e.Code)
}

// Download saves url at path, retrying up to six times. sha, when set, is
// the file's expected SHA-256.
func Download(ctx context.Context, url, path, sha string, progress Progress) error {
	var err error
	for attempt := 1; attempt <= 6; attempt++ {
		if err = once(ctx, url, path, sha, progress); err == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
		}
	}
	return err
}

// retryable reports network failures worth resuming after.
func retryable(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code >= 500
	}
	msg := err.Error()
	for _, s := range []string{"interrupted", "connection reset", "timeout", "EOF", "broken pipe", "no such host", "connection refused"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func once(ctx context.Context, url, path, sha string, progress Progress) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	part := path + ".part"
	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	case http.StatusRequestedRangeNotSatisfiable:
		// The .part file is already complete (or bad): start again.
		os.Remove(part)
		return errors.New("download interrupted: restarting")
	default:
		return &StatusError{URL: url, Code: resp.StatusCode}
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	total := int64(0)
	if resp.ContentLength > 0 {
		total = offset + resp.ContentLength
	}
	w := &counter{done: offset, total: total, progress: progress}
	if progress != nil {
		progress(offset, total)
	}
	_, err = io.Copy(io.MultiWriter(out, w), resp.Body)
	out.Close()
	w.report()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("download interrupted: %w", err)
	}
	if sha != "" {
		if err := Verify(part, sha); err != nil {
			os.Remove(part)
			return err
		}
	}
	return os.Rename(part, path)
}

type counter struct {
	done, total int64
	progress    Progress
	last        time.Time
}

func (c *counter) Write(b []byte) (int, error) {
	c.done += int64(len(b))
	if time.Since(c.last) > 500*time.Millisecond {
		c.last = time.Now()
		c.report()
	}
	return len(b), nil
}

func (c *counter) report() {
	if c.progress != nil {
		c.progress(c.done, c.total)
	}
}

// Verify checks a file's SHA-256.
func Verify(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", filepath.Base(path), got, want)
	}
	return nil
}
