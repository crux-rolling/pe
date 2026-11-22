package ports

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPortsDiscoveryParsingAndDeps(t *testing.T) {
	portsDir := t.TempDir()

	writePort := func(col, name, body string) {
		dir := filepath.Join(portsDir, col, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePort("core", "zlib", `# Description: Compression library
# URL: https://zlib.net
# Maintainer: CRUX Core Team

name=zlib
version=1.3.1
release=1
source=(https://zlib.net/$name-$version.tar.gz)

build() {
	true
}
`)

	writePort("opt", "app", `# Description: Example app using zlib
# URL: https://example.org/app
# Depends on: zlib, missinglib

name=app
version=2.4.0
release=2
source=(https://example.org/$name-$version.tar.xz local.patch)
renames=($name-v$version.tar.xz SKIP)

build() {
	true
}
`)

	entries, err := ListEntries(portsDir, []string{"core", "opt"})
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	wantEntries := []string{"core/zlib", "opt/app"}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("entries = %v, want %v", entries, wantEntries)
	}

	p, err := Find(portsDir, []string{"core", "opt"}, "app")
	if err != nil {
		t.Fatalf("Find(app): %v", err)
	}
	if p.Name != "app" || p.Version != "2.4.0" || p.Release != "2" || p.Collection != "opt" {
		t.Fatalf("unexpected port: %+v", p)
	}
	if p.Description != "Example app using zlib" {
		t.Fatalf("unexpected description: %q", p.Description)
	}
	if !reflect.DeepEqual(p.Depends, []string{"zlib", "missinglib"}) {
		t.Fatalf("unexpected depends: %v", p.Depends)
	}
	wantSrc := []string{"https://example.org/app-2.4.0.tar.xz", "local.patch"}
	if !reflect.DeepEqual(p.Source, wantSrc) {
		t.Fatalf("source = %v, want %v", p.Source, wantSrc)
	}
	wantRenames := []string{"app-v2.4.0.tar.xz", "SKIP"}
	if !reflect.DeepEqual(p.Renames, wantRenames) {
		t.Fatalf("renames = %v, want %v", p.Renames, wantRenames)
	}

	ordered, missing, err := ResolveDeps(portsDir, []string{"core", "opt"}, []string{"app"})
	if err != nil {
		t.Fatalf("ResolveDeps: %v", err)
	}
	if len(ordered) != 2 || ordered[0].Name != "zlib" || ordered[1].Name != "app" {
		t.Fatalf("unexpected dep order: %+v", ordered)
	}
	if !reflect.DeepEqual(missing, []string{"missinglib"}) {
		t.Fatalf("missing = %v, want [missinglib]", missing)
	}
}

func TestRecursiveDeepNestedSymlinkAndGroupDiscovery(t *testing.T) {
	base := t.TempDir()
	portsDir := filepath.Join(base, "ports")

	// Deeply nested port (depth 3: extra/dev/gports/Pkgfile)
	deepDir := filepath.Join(portsDir, "extra", "dev", "gports")
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deepDir, "Pkgfile"), []byte("name=gports\nversion=1.2.3\nrelease=1\nsource=()\nbuild() { true; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Symlinked external collection + symlink cycle guard
	extCol := filepath.Join(base, "external_col", "symport")
	if err := os.MkdirAll(extCol, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extCol, "Pkgfile"), []byte("name=symport\nversion=0.1\nrelease=1\nsource=()\nbuild() { true; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "external_col"), filepath.Join(portsDir, "linked")); err != nil {
		t.Fatal(err)
	}
	// Create a symlink loop back to portsDir to verify cycle protection
	if err := os.Symlink(portsDir, filepath.Join(portsDir, "loop")); err != nil {
		t.Fatal(err)
	}

	// ListEntries must discover both deep port and symlinked port without looping
	entries, err := ListEntries(portsDir, nil)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	wantEntries := []string{"extra/dev/gports", "linked/symport"}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("entries = %v, want %v", entries, wantEntries)
	}

	// Find must locate deeply nested "gports" by bare name and symlinked "symport"
	p, err := Find(portsDir, nil, "gports")
	if err != nil {
		t.Fatalf("Find(gports) failed: %v", err)
	}
	if p.Name != "gports" || p.Collection != "extra/dev" {
		t.Fatalf("unexpected deep port: %+v", p)
	}
	sp, err := Find(portsDir, nil, "symport")
	if err != nil {
		t.Fatalf("Find(symport) failed: %v", err)
	}
	if sp.Name != "symport" || sp.Collection != "linked" {
		t.Fatalf("unexpected symlinked port: %+v", sp)
	}

	// FindGroup must expand a collection/subdirectory ("extra") into all nested ports
	group, err := FindGroup(portsDir, "extra")
	if err != nil {
		t.Fatalf("FindGroup(extra) failed: %v", err)
	}
	if len(group) != 1 || group[0].Name != "gports" {
		t.Fatalf("unexpected FindGroup result: %+v", group)
	}
}
