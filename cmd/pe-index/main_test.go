package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kryx-linux/pe/internal/repo"
)

func TestRunIndexCRUXArchivesAndPrune(t *testing.T) {
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "pkgs")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	portsDir := filepath.Join(dir, "ports")
	if err := os.MkdirAll(filepath.Join(portsDir, "core", "zlib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portsDir, "core", "zlib", "Pkgfile"), []byte(`# Description: Compression library
# Depends on: glibc
name=zlib
version=1.3.1
release=1
source=()
build() { true; }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	a1 := filepath.Join(pkgDir, "zlib#1.3.1-1.pkg.tar.zst")
	a2 := filepath.Join(pkgDir, "curl#8.10.0-1.pkg.tar.xz")
	if err := os.WriteFile(a1, []byte("zst-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a2, []byte("xz-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(dir, "index.json")
	baseURL := "https://github.com/kryx-linux/ports/releases/download/rolling-x86_64"
	var stderr bytes.Buffer
	if err := runIndex(idxPath, pkgDir, portsDir, baseURL, "x86_64", false, &stderr); err != nil {
		t.Fatalf("runIndex failed: %v (%s)", err, stderr.String())
	}

	idx, err := repo.Load(idxPath)
	if err != nil {
		t.Fatalf("Load index: %v", err)
	}
	zlibEntry, ok := repo.Find(idx, "zlib", "x86_64")
	if !ok {
		t.Fatalf("zlib missing from index")
	}
	if zlibEntry.Pkgver != "1.3.1" || zlibEntry.Pkgrel != "1" || zlibEntry.Filename != "zlib#1.3.1-1.pkg.tar.zst" {
		t.Fatalf("unexpected zlib entry: %+v", zlibEntry)
	}
	wantURL := baseURL + "/zlib%231.3.1-1.pkg.tar.zst"
	if zlibEntry.URL != wantURL {
		t.Fatalf("URL = %q, want %q", zlibEntry.URL, wantURL)
	}
	if zlibEntry.Description != "Compression library" || zlibEntry.Collection != "core" {
		t.Fatalf("expected port metadata enrichment, got %+v", zlibEntry)
	}
	if _, ok := repo.Find(idx, "curl", "x86_64"); !ok {
		t.Fatalf("curl (.pkg.tar.xz) missing from index")
	}

	// Remove curl archive and re-run with --prune
	if err := os.Remove(a2); err != nil {
		t.Fatal(err)
	}
	if err := runIndex(idxPath, pkgDir, portsDir, baseURL, "x86_64", true, &stderr); err != nil {
		t.Fatalf("runIndex prune failed: %v", err)
	}
	idx2, err := repo.Load(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := repo.Find(idx2, "curl", "x86_64"); ok {
		t.Fatalf("expected curl to be pruned")
	}
	if _, ok := repo.Find(idx2, "zlib", "x86_64"); !ok {
		t.Fatalf("expected zlib to remain after prune")
	}
}
