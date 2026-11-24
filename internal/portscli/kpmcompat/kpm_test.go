package kpmcompat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyAndParseDB(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, PkgDir)
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	raw := `bash
5.2.37-1
bin/bash
usr/share/man/man1/bash.1.gz

empty-pkg
1.0-1

zlib
1.3.1-1
usr/lib/libz.so.1
`
	if err := os.WriteFile(filepath.Join(root, PkgDB), []byte(raw), 0o444); err != nil {
		t.Fatal(err)
	}

	db, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if len(db.Packages) != 2 {
		t.Fatalf("expected 2 valid packages (ignoring zero-file entry), got %d: %+v", len(db.Packages), db.Packages)
	}
	if db.Packages["bash"].Version != "5.2.37-1" {
		t.Fatalf("bash version = %q", db.Packages["bash"].Version)
	}
	if db.Packages["zlib"].Version != "1.3.1-1" {
		t.Fatalf("zlib version = %q", db.Packages["zlib"].Version)
	}
	names := db.SortedNames()
	if len(names) != 2 || names[0] != "bash" || names[1] != "zlib" {
		t.Fatalf("SortedNames = %v", names)
	}
}

func TestCompareFullVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.3.1-1", "1.3.1-1", 0},
		{"1.3.1-1", "1.3.1-2", -1},
		{"1.3.1-10", "1.3.1-2", 1},
		{"1.10.0-1", "1.2.0-1", 1},
		{"6.18.64-1", "6.18.64-2", -1},
		{"2.0-1", "1.9.9-9", 1},
	}
	for _, tc := range cases {
		if got := CompareFullVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareFullVersion(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
