package builder

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/config"
	"github.com/kryx-linux/kpm/internal/ports"
	"github.com/kryx-linux/kpm/internal/ui"
)

func TestBuildMinimalPkgfile(t *testing.T) {
	base := t.TempDir()
	portsDir := filepath.Join(base, "ports")
	portDir := filepath.Join(portsDir, "opt", "hello")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Local source file + Pkgfile using $CFLAGS and $JOBS from kpm.ini
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

func TestFallbackSourceURLs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{
			in: "https://ftpmirror.gnu.org/ed/ed-1.22.6.tar.lz",
			want: []string{
				"https://ftp.gnu.org/gnu/ed/ed-1.22.6.tar.lz",
				"https://mirrors.kernel.org/gnu/ed/ed-1.22.6.tar.lz",
			},
		},
		{
			in: "https://ftpmirror.gnu.org/gnu/bash/bash-5.3.tar.gz",
			want: []string{
				"https://ftp.gnu.org/gnu/bash/bash-5.3.tar.gz",
				"https://mirrors.kernel.org/gnu/bash/bash-5.3.tar.gz",
			},
		},
		{
			in:   "https://ftp.gnu.org/gnu/ed/ed-1.22.6.tar.lz",
			want: []string{"https://mirrors.kernel.org/gnu/ed/ed-1.22.6.tar.lz"},
		},
		{
			in:   "https://download.savannah.gnu.org/releases/ed/ed-1.22.6.tar.lz",
			want: []string{"https://download-mirror.savannah.gnu.org/releases/ed/ed-1.22.6.tar.lz"},
		},
		{
			in:   "https://example.org/foo.tar.xz",
			want: nil,
		},
	}

	for _, tc := range cases {
		got := fallbackSourceURLs(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("fallbackSourceURLs(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("fallbackSourceURLs(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestHTTPDownloadWithRetryRecoversFromTransientMirrorFailure(t *testing.T) {
	prevDelay := downloadRetryDelay
	downloadRetryDelay = 0
	t.Cleanup(func() { downloadRetryDelay = prevDelay })

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			http.Error(w, "mirror unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("tarball-bytes"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "ed-1.22.6.tar.lz")
	var out bytes.Buffer
	err := httpDownloadWithRetry(srv.URL+"/ed-1.22.6.tar.lz", dst, 5*time.Second, 3, &out, ui.NewStyler("never", &out))
	if err != nil {
		t.Fatalf("expected retry to recover on attempt 3, got err=%v\nlog:\n%s", err, out.String())
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 HTTP attempts, got %d", calls.Load())
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "tarball-bytes" {
		t.Fatalf("unexpected downloaded content %q (err=%v)", string(got), err)
	}
}
