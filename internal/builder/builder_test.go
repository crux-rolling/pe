package builder

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryx-linux/pe/internal/archive"
	"github.com/kryx-linux/pe/internal/config"
	"github.com/kryx-linux/pe/internal/ports"
)

func TestBuildMinimalPkgfile(t *testing.T) {
	base := t.TempDir()
	portsDir := filepath.Join(base, "ports")
	portDir := filepath.Join(portsDir, "opt", "hello")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Local source file + Pkgfile using $CFLAGS and $JOBS from pe.ini
	if err := os.WriteFile(filepath.Join(portDir, "hello.sh"), []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgfile := `# Description: Hello test port
# URL: https://example.org/hello
# Maintainer: Test

name=hello
version=1.0.0
release=1
source=(hello.sh)

build() {
	test "$JOBS" = "2"
	test "$CFLAGS" = "-O2 -pipe"
	install -D -m 0755 "$SRC/hello.sh" "$PKG/usr/bin/hello"
	mkdir -p "$PKG/usr/share/man/man1"
	printf '.TH HELLO 1\n' > "$PKG/usr/share/man/man1/hello.1"
}
`
	if err := os.WriteFile(filepath.Join(portDir, "Pkgfile"), []byte(pkgfile), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Paths.PortsDir = portsDir
	cfg.Paths.SourceCache = filepath.Join(base, "sources")
	cfg.Paths.PackageDir = filepath.Join(base, "packages")
	cfg.Paths.WorkDir = filepath.Join(base, "work")
	cfg.Build.Jobs = 2
	cfg.Build.CFLAGS = "-O2 -pipe"
	cfg.Build.CXXFLAGS = "-O2 -pipe"
	cfg.Build.MAKEFLAGS = "-j2"
	cfg.Build.Compression = "zst"
	cfg.Build.IgnoreMD5Sum = true
	cfg.Build.IgnoreSignature = true

	p, err := ports.Find(portsDir, []string{"opt"}, "hello")
	if err != nil {
		t.Fatalf("Find port: %v", err)
	}

	var out, errOut bytes.Buffer
	res, err := Build(cfg, p, Options{
		Out:    &out,
		ErrOut: &errOut,
	})
	if err != nil {
		t.Fatalf("Build failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}

	wantArchive := filepath.Join(cfg.Paths.PackageDir, "hello#1.0.0-1.pkg.tar.zst")
	if res.ArchivePath != wantArchive {
		t.Fatalf("ArchivePath = %q, want %q", res.ArchivePath, wantArchive)
	}
	name, info, err := archive.Open(res.ArchivePath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	if name != "hello" || info.Version != "1.0.0-1" {
		t.Fatalf("unexpected archive metadata: %q %q", name, info.Version)
	}
	if _, ok := info.Files["usr/bin/hello"]; !ok {
		t.Fatalf("expected usr/bin/hello in built archive: %v", info.SortedFiles())
	}
	if _, ok := info.Files["usr/share/man/man1/hello.1.gz"]; !ok {
		t.Fatalf("expected compressed manpage usr/share/man/man1/hello.1.gz in built archive: %v", info.SortedFiles())
	}
	// .footprint should have been generated automatically on first build
	fpData, err := os.ReadFile(filepath.Join(portDir, ".footprint"))
	if err != nil || !strings.Contains(string(fpData), "usr/bin/hello") {
		t.Fatalf("expected generated .footprint containing usr/bin/hello, err=%v", err)
	}
	// Second build without --force should report up to date
	res2, err := Build(cfg, p, Options{Out: &out, ErrOut: &errOut})
	if err != nil {
		t.Fatalf("second Build failed: %v", err)
	}
	if !res2.UpToDate {
		t.Fatalf("expected second build to report up to date")
	}
}
