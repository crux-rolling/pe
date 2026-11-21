package repo

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndexMergePruneAndSchemaCompatibility(t *testing.T) {
	idx := New()
	Merge(&idx,
		Entry{
			Pkgname:  "zlib",
			Pkgver:   "1.2.13",
			Pkgrel:   "1",
			Arch:     "x86_64",
			Filename: "zlib#1.2.13-1.pkg.tar.zst",
			URL:      "https://github.com/ezke5/packages/releases/download/rolling-x86_64/zlib%231.2.13-1.pkg.tar.zst",
			SHA256:   "aaa",
			Size:     100,
		},
		Entry{
			Pkgname:  "zlib",
			Pkgver:   "1.3.1",
			Pkgrel:   "1",
			Arch:     "x86_64",
			Filename: "zlib#1.3.1-1.pkg.tar.zst",
			URL:      "https://github.com/ezke5/packages/releases/download/rolling-x86_64/zlib%231.3.1-1.pkg.tar.zst",
			SHA256:   "bbb",
			Size:     200,
		},
	)

	if idx.Packages["zlib"].Latest != "1.3.1-1" {
		t.Fatalf("expected latest 1.3.1-1, got %q", idx.Packages["zlib"].Latest)
	}
	entry, ok := Find(idx, "zlib", "x86_64")
	if !ok || entry.Pkgver != "1.3.1" {
		t.Fatalf("Find returned %+v, %v", entry, ok)
	}

	// Verify serialized JSON contains expected field names and zero ANRIX/USE keys.
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(data)
	for _, key := range []string{`"schema_version"`, `"packages"`, `"latest"`, `"versions"`, `"pkgname"`, `"pkgver"`, `"pkgrel"`, `"arch"`, `"filename"`, `"url"`, `"sha256"`, `"size"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("expected key %s in JSON: %s", key, raw)
		}
	}
	for _, forbidden := range []string{`"use_flags"`, `"use_disabled"`, `"use_hash"`, `"use_tags"`} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("unexpected ANRIX/USE key %s in JSON: %s", forbidden, raw)
		}
	}

	removed := Prune(&idx, map[string]bool{"zlib#1.3.1-1.pkg.tar.zst": true})
	if removed != 1 || len(idx.Packages["zlib"].Versions) != 1 {
		t.Fatalf("Prune removed=%d versions=%d", removed, len(idx.Packages["zlib"].Versions))
	}
}

func TestCompareVersionNumericSegments(t *testing.T) {
	if CompareVersion("1.2.9", "1", "1.2.10", "1") >= 0 {
		t.Fatalf("expected 1.2.9-1 < 1.2.10-1")
	}
	if CompareVersion("1.3.1", "1", "1.3.1", "2") >= 0 {
		t.Fatalf("expected 1.3.1-1 < 1.3.1-2")
	}
	if CompareVersion("2.0", "1", "2.0", "1") != 0 {
		t.Fatalf("expected equal versions")
	}
}

func TestSyncAndFetchBinary(t *testing.T) {
	payload := []byte("fake-crux-package-bytes")
	tmpFile := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(tmpFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := SHA256(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			idx := New()
			Merge(&idx, Entry{
				Pkgname:  "demo",
				Pkgver:   "1.0",
				Pkgrel:   "1",
				Arch:     "x86_64",
				Filename: "demo#1.0-1.pkg.tar.zst",
				URL:      srv.URL + "/demo.pkg.tar.zst",
				SHA256:   sum,
				Size:     int64(len(payload)),
			})
			_ = json.NewEncoder(w).Encode(idx)
		case "/demo.pkg.tar.zst":
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	idxPath := filepath.Join(cacheDir, "index.json")
	idx, err := Sync(srv.URL+"/index.json", idxPath, 10*time.Second)
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	entry, ok := Find(idx, "demo", "x86_64")
	if !ok {
		t.Fatalf("demo not found in synced index")
	}
	var out bytes.Buffer
	gotPath, err := FetchBinary(entry, filepath.Join(cacheDir, "bin"), 10*time.Second, &out)
	if err != nil {
		t.Fatalf("FetchBinary failed: %v", err)
	}
	gotBytes, err := os.ReadFile(gotPath)
	if err != nil || !bytes.Equal(gotBytes, payload) {
		t.Fatalf("downloaded payload mismatch: err=%v", err)
	}
}
