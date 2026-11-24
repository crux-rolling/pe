package sync

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

// syncTarball fetches an HTTP(S) .tar or .tar.gz/.tgz ports archive natively
// using Go's standard library and atomically replaces the destination directory.
func syncTarball(ctx context.Context, client *http.Client, src sources.Source) (SyncOutcome, error) {
	if client == nil {
		client = NewHTTPClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("build tarball request: %w", err)
	}
	req.Header.Set("User-Agent", httpupUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return SyncOutcome{}, sanitizeHTTPError("fetching tarball", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SyncOutcome{}, fmt.Errorf("tarball URL returned HTTP %d (%s)", resp.StatusCode, src.URL)
	}

	var reader io.Reader = resp.Body
	lowerURL := strings.ToLower(src.URL)
	isGzip := strings.HasSuffix(lowerURL, ".gz") || strings.HasSuffix(lowerURL, ".tgz") ||
		strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "gzip")
	if isGzip {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return SyncOutcome{}, fmt.Errorf("decompress gzip tarball %s: %w", src.URL, err)
		}
		defer gz.Close()
		reader = gz
	}

	parent := filepath.Dir(src.DestDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return SyncOutcome{}, fmt.Errorf("create parent directory %s: %w", parent, err)
	}
	stageDir, err := os.MkdirTemp(parent, ".ports-tar-stage-*")
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	tr := tar.NewReader(reader)
	fileCount := 0
	for {
		if ctx.Err() != nil {
			return SyncOutcome{}, errors.New("sync canceled")
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return SyncOutcome{}, fmt.Errorf("read tarball %s: %w", src.URL, err)
		}
		clean := path.Clean(strings.TrimPrefix(strings.ReplaceAll(hdr.Name, "\\", "/"), "/"))
		if clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		dst := filepath.Join(stageDir, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return SyncOutcome{}, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return SyncOutcome{}, err
			}
			mode := os.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return SyncOutcome{}, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return SyncOutcome{}, err
			}
			if err := f.Close(); err != nil {
				return SyncOutcome{}, err
			}
			fileCount++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return SyncOutcome{}, err
			}
			_ = os.RemoveAll(dst)
			if err := os.Symlink(hdr.Linkname, dst); err != nil {
				return SyncOutcome{}, err
			}
		}
	}

	// If the tarball had a single top-level wrapper directory (e.g. GitHub archive), unwrap it.
	extractRoot := stageDir
	if entries, err := os.ReadDir(stageDir); err == nil && len(entries) == 1 && entries[0].IsDir() {
		candidate := filepath.Join(stageDir, entries[0].Name())
		// Only unwrap if it's not itself a single port directory containing Pkgfile directly.
		if _, statErr := os.Stat(filepath.Join(candidate, "Pkgfile")); os.IsNotExist(statErr) {
			extractRoot = candidate
		}
	}

	backup := src.DestDir + ".ports-old"
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(src.DestDir); err == nil {
		if err := os.Rename(src.DestDir, backup); err != nil {
			return SyncOutcome{}, fmt.Errorf("move old destination aside: %w", err)
		}
	}
	if err := os.Rename(extractRoot, src.DestDir); err != nil {
		_ = os.Rename(backup, src.DestDir)
		return SyncOutcome{}, fmt.Errorf("swap tarball into %s: %w", src.DestDir, err)
	}
	_ = os.RemoveAll(backup)

	return SyncOutcome{Detail: fmt.Sprintf("extracted %d files", fileCount)}, nil
}
