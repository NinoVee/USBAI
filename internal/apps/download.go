package apps

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// downloadFile saves url to dst, trying a few times if the connection drops.
func downloadFile(url, dst string) error {
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		if err = downloadOnce(url, dst); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt*attempt) * time.Second)
	}
	return err
}

func downloadOnce(url, dst string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst + ".part")
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(dst+".part", dst)
}

// downloadExtract downloads a .tar.gz or .zip and unpacks it into dir,
// leaving out the archive's top folder (as GitHub archives have one).
func downloadExtract(url, dir string) error {
	tmp, err := os.MkdirTemp("", "gangai-dl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	archive := filepath.Join(tmp, "archive")
	if err := downloadFile(url, archive); err != nil {
		return err
	}
	if strings.HasSuffix(strings.ToLower(url), ".zip") {
		return unzipStrip(archive, dir)
	}
	return untarStrip(archive, dir)
}

// stripTop removes an archive path's first folder and checks the rest
// stays inside dir.
func stripTop(dir, name string) (string, bool, error) {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	_, rest, ok := strings.Cut(name, "/")
	if !ok || rest == "" {
		return "", false, nil
	}
	p := filepath.Join(dir, filepath.FromSlash(rest))
	if !strings.HasPrefix(p, filepath.Clean(dir)+string(os.PathSeparator)) {
		return "", false, fmt.Errorf("unsafe path in archive: %s", name)
	}
	return p, true, nil
}

func untarStrip(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		p, ok, err := stripTop(dir, h.Name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			if err := writeFrom(p, tr, os.FileMode(h.Mode)); err != nil {
				return err
			}
		}
		// Links are left out: exFAT can't hold them.
	}
}

func unzipStrip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		p, ok, err := stripTop(dir, f.Name)
		if err != nil {
			return err
		}
		if !ok || f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(p, 0o755)
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFrom(p, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFrom(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm()|0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
