package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePkgFilename(t *testing.T) {
	tests := []struct {
		in      string
		name    string
		ver     string
		ext     string
		ok      bool
		wantVer string
		wantRel string
	}{
		{"/tmp/zlib#1.3.1-1.pkg.tar.zst", "zlib", "1.3.1-1", "zst", true, "1.3.1", "1"},
		{"gcc-libs#14.2.0-2.pkg.tar.xz", "gcc-libs", "14.2.0-2", "xz", true, "14.2.0", "2"},
		{"foo#2026.01-1.pkg.tar.gz", "foo", "2026.01-1", "gz", true, "2026.01", "1"},
		{"bar#1.0-1.pkg.tar.bz2", "bar", "1.0-1", "bz2", true, "1.0", "1"},
		{"baz#1.0-1.pkg.tar.lz", "baz", "1.0-1", "lz", true, "1.0", "1"},
		{"invalid-1.0-1.pkg.tar.zst", "", "", "", false, "", ""},
		{"foo#1.0-1.pkg.tar.rar", "", "", "", false, "", ""},
	}
	for _, tc := range tests {
		name, ver, ext, ok := ParsePkgFilename(tc.in)
		if ok != tc.ok || name != tc.name || ver != tc.ver || ext != tc.ext {
			t.Errorf("ParsePkgFilename(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.in, name, ver, ext, ok, tc.name, tc.ver, tc.ext, tc.ok)
		}
		if ok {
			v, r, rok := SplitVersionRelease(ver)
			if !rok || v != tc.wantVer || r != tc.wantRel {
				t.Errorf("SplitVersionRelease(%q) = (%q,%q,%v), want (%q,%q,true)", ver, v, r, rok, tc.wantVer, tc.wantRel)
			}
		}
	}
}

func TestCreateOpenFootprintAndExtractRejected(t *testing.T) {
	stage := t.TempDir()
	if err := os.MkdirAll(filepath.Join(stage, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stage, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "etc/demo.conf"), []byte("upstream-v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "etc/same.conf"), []byte("identical\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "usr/bin/demo"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "usr/bin/empty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("demo", filepath.Join(stage, "usr/bin/demo-link")); err != nil {
		t.Fatal(err)
	}

	pkgPath := filepath.Join(t.TempDir(), "demo#1.0-1.pkg.tar.zst")
	if err := Create(stage, pkgPath, "zst"); err != nil {
		t.Fatalf("Create zst archive: %v", err)
	}

	name, info, err := Open(pkgPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if name != "demo" || info.Version != "1.0-1" {
		t.Fatalf("Open got (%q, %q)", name, info.Version)
	}
	if _, ok := info.Files["usr/bin/demo"]; !ok {
		t.Fatalf("expected usr/bin/demo in archive files: %v", info.SortedFiles())
	}

	var fp bytes.Buffer
	if err := Footprint(pkgPath, &fp); err != nil {
		t.Fatalf("Footprint: %v", err)
	}
	fpStr := fp.String()
	if !strings.Contains(fpStr, "lrwxrwxrwx") || !strings.Contains(fpStr, "usr/bin/demo-link -> demo") {
		t.Fatalf("expected symlink in footprint, got:\n%s", fpStr)
	}
	if !strings.Contains(fpStr, "usr/bin/empty (EMPTY)") {
		t.Fatalf("expected (EMPTY) marker in footprint, got:\n%s", fpStr)
	}

	// Test rejected behavior on upgrade:
	// - etc/demo.conf differs -> kept in place and new version written to var/lib/pkg/rejected/etc/demo.conf
	// - etc/same.conf identical -> pruned from rejected/
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/demo.conf"), []byte("local-edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/same.conf"), []byte("identical\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	keepList := map[string]struct{}{
		"etc/demo.conf": {},
		"etc/same.conf": {},
	}
	var out, errOut bytes.Buffer
	if err := Extract(pkgPath, root, keepList, nil, true, &out, &errOut); err != nil {
		t.Fatalf("Extract: %v (stderr: %s)", err, errOut.String())
	}

	gotLocal, _ := os.ReadFile(filepath.Join(root, "etc/demo.conf"))
	if string(gotLocal) != "local-edited\n" {
		t.Fatalf("expected existing etc/demo.conf to be preserved, got %q", string(gotLocal))
	}
	gotRej, err := os.ReadFile(filepath.Join(root, "var/lib/pkg/rejected/etc/demo.conf"))
	if err != nil || string(gotRej) != "upstream-v2\n" {
		t.Fatalf("expected rejected file with upstream-v2, got %q (err=%v)", string(gotRej), err)
	}
	if _, err := os.Lstat(filepath.Join(root, "var/lib/pkg/rejected/etc/same.conf")); !os.IsNotExist(err) {
		t.Fatalf("expected identical rejected file etc/same.conf to be pruned")
	}
	if !strings.Contains(out.String(), "rejecting etc/demo.conf") {
		t.Fatalf("expected rejection log message, got %q", out.String())
	}
}
