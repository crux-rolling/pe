package pkgops

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/pkgdb"
)

func buildTestPkg(t *testing.T, dir, name, verRel string, files map[string]string) string {
	t.Helper()
	stage := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outPath := filepath.Join(dir, name+"#"+verRel+".pkg.tar.zst")
	if err := archive.Create(stage, outPath, "zst"); err != nil {
		t.Fatalf("Create test pkg: %v", err)
	}
	return outPath
}

func TestInstallUpgradeConflictAndRemove(t *testing.T) {
	root := t.TempDir()
	if err := pkgdb.EnsureInitialized(root); err != nil {
		t.Fatal(err)
	}
	// Write pkgadd.conf protecting etc/
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/pkgadd.conf"), []byte("UPGRADE ^etc/.*$ NO\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgDir := t.TempDir()
	v1 := buildTestPkg(t, pkgDir, "hello", "1.0-1", map[string]string{
		"usr/bin/hello":  "v1\n",
		"etc/hello.conf": "default-v1\n",
	})
	v2 := buildTestPkg(t, pkgDir, "hello", "2.0-1", map[string]string{
		"usr/bin/hello":  "v2\n",
		"usr/bin/extra":  "extra\n",
		"etc/hello.conf": "default-v2\n",
	})
	conflictPkg := buildTestPkg(t, pkgDir, "other", "1.0-1", map[string]string{
		"usr/bin/hello": "conflict\n",
	})

	var out, errOut bytes.Buffer
	baseOpts := InstallOptions{
		Root:          root,
		SkipRootCheck: true,
		SkipLdconfig:  true,
		Out:           &out,
		ErrOut:        &errOut,
	}

	// 1. Fresh install
	name, ver, err := InstallArchive(v1, baseOpts)
	if err != nil {
		t.Fatalf("fresh install failed: %v", err)
	}
	if name != "hello" || ver != "1.0-1" {
		t.Fatalf("got (%q, %q)", name, ver)
	}

	// 2. Duplicate install without --upgrade fails
	if _, _, err := InstallArchive(v1, baseOpts); err == nil {
		t.Fatalf("expected error on reinstall without --upgrade")
	}

	// 3. Conflicting package fails without --force
	if _, _, err := InstallArchive(conflictPkg, baseOpts); err == nil {
		t.Fatalf("expected conflict error without --force")
	}

	// 4. Modify etc/hello.conf locally and upgrade to v2
	if err := os.WriteFile(filepath.Join(root, "etc/hello.conf"), []byte("user-custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	upgOpts := baseOpts
	upgOpts.Upgrade = true
	if _, ver, err := InstallArchive(v2, upgOpts); err != nil || ver != "2.0-1" {
		t.Fatalf("upgrade failed: ver=%q err=%v", ver, err)
	}

	// Binary updated, config preserved, new config placed in var/lib/pkg/rejected/
	binData, _ := os.ReadFile(filepath.Join(root, "usr/bin/hello"))
	if string(binData) != "v2\n" {
		t.Fatalf("expected usr/bin/hello v2, got %q", string(binData))
	}
	cfgData, _ := os.ReadFile(filepath.Join(root, "etc/hello.conf"))
	if string(cfgData) != "user-custom\n" {
		t.Fatalf("expected preserved etc/hello.conf, got %q", string(cfgData))
	}
	rejData, err := os.ReadFile(filepath.Join(root, "var/lib/pkg/rejected/etc/hello.conf"))
	if err != nil || string(rejData) != "default-v2\n" {
		t.Fatalf("expected rejected etc/hello.conf, got %q err=%v", string(rejData), err)
	}

	// 5. Remove package
	if err := RemovePackage("hello", RemoveOptions{
		Root:          root,
		SkipRootCheck: true,
		SkipLdconfig:  true,
		ErrOut:        &errOut,
	}); err != nil {
		t.Fatalf("RemovePackage failed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "usr/bin/hello")); !os.IsNotExist(err) {
		t.Fatalf("expected usr/bin/hello to be removed")
	}
	db, err := pkgdb.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if db.FindPkg("hello") {
		t.Fatalf("expected hello removed from DB")
	}
}

func TestInstallInitializesMissingPkgDB(t *testing.T) {
	freshRoot := filepath.Join(t.TempDir(), "fresh-root")
	pkgDir := t.TempDir()
	pkg := buildTestPkg(t, pkgDir, "zig", "0.17.1-1", map[string]string{
		"usr/bin/zig": "zig-binary\n",
	})

	var out, errOut bytes.Buffer
	name, ver, err := InstallArchive(pkg, InstallOptions{
		Root:          freshRoot,
		SkipRootCheck: true,
		SkipLdconfig:  true,
		Out:           &out,
		ErrOut:        &errOut,
	})
	if err != nil {
		t.Fatalf("InstallArchive on uninitialized root failed: %v", err)
	}
	if name != "zig" || ver != "0.17.1-1" {
		t.Fatalf("got (%q, %q)", name, ver)
	}
	db, err := pkgdb.Open(freshRoot)
	if err != nil {
		t.Fatalf("Open initialized db: %v", err)
	}
	if !db.FindPkg("zig") {
		t.Fatalf("expected zig registered in newly initialized DB")
	}
}

func TestConflictTruncationAndVerbose(t *testing.T) {
	root := t.TempDir()
	files := make(map[string]string, 30)
	for i := 0; i < 30; i++ {
		rel := filepath.Join("usr/lib/big", string(rune('a'+(i/26)))+string(rune('a'+(i%26)))+".txt")
		files[rel] = "data\n"
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("existing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg := buildTestPkg(t, t.TempDir(), "big", "1.0-1", files)

	var errBuf bytes.Buffer
	_, _, err := InstallArchive(pkg, InstallOptions{
		Root:          root,
		SkipRootCheck: true,
		SkipLdconfig:  true,
		ErrOut:        &errBuf,
	})
	if err == nil {
		t.Fatalf("expected conflict error")
	}
	lines := bytes.Split(bytes.TrimSpace(errBuf.Bytes()), []byte("\n"))
	if len(lines) != DefaultMaxConflictsDisplayed+1 {
		t.Fatalf("expected %d truncated lines + summary, got %d:\n%s", DefaultMaxConflictsDisplayed+1, len(lines), errBuf.String())
	}

	errBuf.Reset()
	_, _, err = InstallArchive(pkg, InstallOptions{
		Root:          root,
		Verbose:       true,
		SkipRootCheck: true,
		SkipLdconfig:  true,
		ErrOut:        &errBuf,
	})
	if err == nil {
		t.Fatalf("expected conflict error in verbose mode")
	}
	lines = bytes.Split(bytes.TrimSpace(errBuf.Bytes()), []byte("\n"))
	if len(lines) != 30 {
		t.Fatalf("expected all 30 conflict lines in verbose mode, got %d", len(lines))
	}
}
