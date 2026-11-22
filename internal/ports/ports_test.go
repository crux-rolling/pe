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

func TestDirNameMismatchPkgfileNameAndResolveDeps(t *testing.T) {
	portsDir := t.TempDir()

	writePort := func(relDir, body string) {
		dir := filepath.Join(portsDir, filepath.FromSlash(relDir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePort("core/bc", "name=bc\nversion=1.07\nrelease=1\nsource=()\nbuild() { true; }\n")
	writePort("rolling/linux-6.18", "# Depends on: bc\nname=kernel\nversion=6.18.64\nrelease=2\nsource=()\nbuild() { true; }\n")
	writePort("rolling/meta-boot", "# Depends on: kernel\nname=meta-boot\nversion=1.0\nrelease=1\nsource=()\nbuild() { true; }\n")

	for _, query := range []string{"linux-6.18", "rolling/linux-6.18", "kernel", "rolling/kernel"} {
		p, err := Find(portsDir, nil, query)
		if err != nil {
			t.Fatalf("Find(%q) failed: %v", query, err)
		}
		if p.Name != "kernel" || p.Collection != "rolling" || p.RelDir != "rolling/linux-6.18" {
			t.Fatalf("Find(%q) = %+v", query, p)
		}
	}

	root, err := Find(portsDir, nil, "linux-6.18")
	if err != nil {
		t.Fatalf("Find(linux-6.18): %v", err)
	}
	ordered, missing, err := ResolveDepsForPorts(portsDir, nil, []Port{root})
	if err != nil {
		t.Fatalf("ResolveDepsForPorts(linux-6.18): %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("unexpected missing deps: %v", missing)
	}
	if len(ordered) != 2 || ordered[0].Name != "bc" || ordered[1].Name != "kernel" {
		t.Fatalf("unexpected order for linux-6.18: %+v", ordered)
	}

	orderedMeta, missingMeta, err := ResolveDeps(portsDir, nil, []string{"meta-boot"})
	if err != nil {
		t.Fatalf("ResolveDeps(meta-boot): %v", err)
	}
	if len(missingMeta) != 0 {
		t.Fatalf("unexpected missing deps for meta-boot: %v", missingMeta)
	}
	if len(orderedMeta) != 3 || orderedMeta[0].Name != "bc" || orderedMeta[1].Name != "kernel" || orderedMeta[2].Name != "meta-boot" {
		t.Fatalf("unexpected order for meta-boot: %+v", orderedMeta)
	}
}

func TestMakeDependsAndBinaryFirstBuildPlan(t *testing.T) {
	portsDir := t.TempDir()
	writePort := func(relDir, body string) {
		dir := filepath.Join(portsDir, filepath.FromSlash(relDir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePort("core/glibc", "name=glibc\nversion=2.40\nrelease=1\nsource=()\nbuild() { true; }\n")
	writePort("opt/go-bootstrap", "name=go-bootstrap\nversion=1.22\nrelease=1\nsource=()\nbuild() { true; }\n")
	writePort("opt/go", "# Depends on: glibc\n# Make depends on: go-bootstrap\nname=go\nversion=1.26\nrelease=1\nsource=()\nbuild() { true; }\n")
	writePort("rolling/app", "# Depends on: glibc\n# Make depends on: go, bin-only-tool\nname=app\nversion=1.0\nrelease=1\nsource=()\nbuild() { true; }\n")

	app, err := Find(portsDir, nil, "app")
	if err != nil {
		t.Fatalf("Find(app): %v", err)
	}
	if !reflect.DeepEqual(app.Depends, []string{"glibc"}) {
		t.Fatalf("Depends = %v, want [glibc]", app.Depends)
	}
	if !reflect.DeepEqual(app.MakeDepends, []string{"go", "bin-only-tool"}) {
		t.Fatalf("MakeDepends = %v, want [go bin-only-tool]", app.MakeDepends)
	}

	findPort := func(name string) (Port, error) {
		return Find(portsDir, nil, name)
	}
	installed := map[string]bool{"glibc": true}

	// 1. Binary-first plan: `go` and `bin-only-tool` exist in binary index.
	//    `go-bootstrap` must NOT be scheduled because `go` comes from binary!
	binLookup := func(name string) ([]string, bool) {
		switch name {
		case "go":
			return []string{"glibc"}, true
		case "bin-only-tool":
			return nil, true
		default:
			return nil, false
		}
	}

	plan, missing, err := ResolveBuildPlan([]Port{app}, findPort, installed, binLookup)
	if err != nil {
		t.Fatalf("ResolveBuildPlan(binary): %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected 0 missing deps, got %v", missing)
	}
	// Expect: glibc (AlreadyInstalled), go (FromBinary), bin-only-tool (FromBinary), app (IsTarget)
	if len(plan) != 4 {
		t.Fatalf("expected 4 steps, got %+v", plan)
	}
	if plan[0].Name != "glibc" || !plan[0].AlreadyInstalled ||
		plan[1].Name != "go" || !plan[1].FromBinary ||
		plan[2].Name != "bin-only-tool" || !plan[2].FromBinary ||
		plan[3].Name != "app" || !plan[3].IsTarget {
		t.Fatalf("unexpected binary plan: %+v", plan)
	}

	// 2. Source-only plan (--source-deps, lookupBinary=nil):
	//    `go` must pull its own MakeDepends (`go-bootstrap`), while `bin-only-tool` is reported missing.
	srcPlan, srcMissing, err := ResolveBuildPlan([]Port{app}, findPort, installed, nil)
	if err != nil {
		t.Fatalf("ResolveBuildPlan(source): %v", err)
	}
	if !reflect.DeepEqual(srcMissing, []string{"bin-only-tool"}) {
		t.Fatalf("expected [bin-only-tool] missing in source-only mode, got %v", srcMissing)
	}
	if len(srcPlan) != 4 || srcPlan[0].Name != "glibc" || !srcPlan[0].AlreadyInstalled ||
		srcPlan[1].Name != "go-bootstrap" || srcPlan[1].FromBinary ||
		srcPlan[2].Name != "go" || srcPlan[2].FromBinary ||
		srcPlan[3].Name != "app" || !srcPlan[3].IsTarget {
		t.Fatalf("unexpected source plan: %+v", srcPlan)
	}
}
