package sync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/gokrazy/rsync/rsyncd"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestParseRepoManifestVariants(t *testing.T) {
	pkgHash := md5Hex("name=foo\n")
	fpHash := md5Hex("usr/bin/foo\n")
	sigHash := md5Hex("sig\n")

	raw := strings.Join([]string{
		"# comment",
		"d:.",
		"d:foo",
		"d:0755:bar",
		"f:" + pkgHash + ":foo/Pkgfile",
		"f:0644:" + fpHash + ":foo/.footprint",
		"f:0644:4:" + sigHash + ":bar/.signature",
		"f:bar/post-install",
	}, "\n")

	m, err := ParseRepoManifest(strings.NewReader(raw), "")
	if err != nil {
		t.Fatalf("ParseRepoManifest failed: %v", err)
	}
	if len(m.Dirs) != 2 || len(m.Files) != 4 {
		t.Fatalf("unexpected manifest counts: dirs=%d files=%d", len(m.Dirs), len(m.Files))
	}
}

func TestHTTPUpSyncDiffAndPrune(t *testing.T) {
	var mu stdsync.Mutex
	files := map[string]string{
		"alpha/Pkgfile":    "name=alpha\nversion=1.0\n",
		"alpha/.footprint": "usr/bin/alpha\n",
		"beta/Pkgfile":     "name=beta\nversion=1.0\n",
	}

	buildRepo := func() string {
		var lines []string
		dirs := map[string]bool{}
		for p, content := range files {
			d := filepath.Dir(p)
			if !dirs[d] {
				dirs[d] = true
				lines = append(lines, "d:0755:"+d)
			}
			lines = append(lines, fmt.Sprintf("f:0644:%s:%s", md5Hex(content), p))
		}
		return strings.Join(lines, "\n") + "\n"
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if rel == "REPO" {
			_, _ = w.Write([]byte(buildRepo()))
			return
		}
		if body, ok := files[rel]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "hamzaports")
	src := sources.Source{
		Protocol:   sources.ProtoHTTPUp,
		Collection: "hamzaports",
		URL:        srv.URL + "/",
		DestDir:    dest,
	}

	out1, err := syncHTTPUp(context.Background(), srv.Client(), src)
	if err != nil || out1.Skipped {
		t.Fatalf("initial sync failed: out=%+v err=%v", out1, err)
	}
	out2, err := syncHTTPUp(context.Background(), srv.Client(), src)
	if err != nil || !out2.Skipped {
		t.Fatalf("expected skip on second sync: out=%+v err=%v", out2, err)
	}

	mu.Lock()
	files["alpha/Pkgfile"] = "name=alpha\nversion=2.0\n"
	delete(files, "beta/Pkgfile")
	mu.Unlock()

	out3, err := syncHTTPUp(context.Background(), srv.Client(), src)
	if err != nil || out3.Skipped {
		t.Fatalf("expected update+prune on third sync: out=%+v err=%v", out3, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "beta")); !os.IsNotExist(err) {
		t.Fatalf("expected beta/ to be pruned")
	}
}

func TestPureGoGitSyncAndAutoRecovery(t *testing.T) {
	// Build a local upstream Git repository purely via go-git (no git binary required).
	root := t.TempDir()
	upstreamDir := filepath.Join(root, "upstream")

	repo, err := gogit.PlainInit(upstreamDir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	commitFile := func(rel, content, msg string) {
		t.Helper()
		full := filepath.Join(upstreamDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Commit(msg, &gogit.CommitOptions{
			Author: &object.Signature{
				Name:  "Test",
				Email: "test@example.com",
				When:  time.Now(),
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	commitFile("bash/Pkgfile", "name=bash\nversion=5.2\nrelease=1\n", "initial commit")

	dest := filepath.Join(root, "ports", "core")
	src := sources.Source{
		Protocol:   sources.ProtoGit,
		Collection: "core",
		URL:        "file://" + upstreamDir,
		DestDir:    dest,
	}

	// 1. Fresh pure-Go shallow clone
	out1, err := syncGit(context.Background(), src)
	if err != nil || out1.Skipped || !strings.Contains(out1.Detail, "cloned") {
		t.Fatalf("fresh pure-Go git sync failed: out=%+v err=%v", out1, err)
	}

	// 2. Up-to-date skip
	out2, err := syncGit(context.Background(), src)
	if err != nil || !out2.Skipped {
		t.Fatalf("expected up-to-date skip: out=%+v err=%v", out2, err)
	}

	// 3. Upstream commit -> pure-Go fetch + reset
	commitFile("bash/Pkgfile", "name=bash\nversion=5.3\nrelease=1\n", "bump bash 5.3")
	out3, err := syncGit(context.Background(), src)
	if err != nil || out3.Skipped {
		t.Fatalf("expected update after upstream commit: out=%+v err=%v", out3, err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "bash", "Pkgfile"))
	if !strings.Contains(string(got), "version=5.3") {
		t.Fatalf("expected updated Pkgfile, got %q", string(got))
	}

	// 4. Corrupt .git/HEAD and index -> automatic pure-Go recovery
	_ = os.WriteFile(filepath.Join(dest, ".git", "HEAD"), []byte("corrupted\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dest, ".git", "index"), []byte("corrupted"), 0o644)
	out4, err := syncGit(context.Background(), src)
	if err != nil || !strings.Contains(out4.Detail, "recovered") {
		t.Fatalf("expected recovery from corrupt git: out=%+v err=%v", out4, err)
	}

	// 5. InspectCollection without git binary
	st := InspectCollection(context.Background(), src)
	if !st.Exists || st.PortCount != 1 || !strings.Contains(st.State, "synced @") {
		t.Fatalf("unexpected InspectCollection state: %+v", st)
	}
}

func TestPureGoRsyncDaemonSync(t *testing.T) {
	// Serve a local ports tree over a real TCP loopback socket using gokrazy/rsync's pure-Go daemon.
	root := t.TempDir()
	modDir := filepath.Join(root, "mod-contrib")
	if err := os.MkdirAll(filepath.Join(modDir, "curl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "curl", "Pkgfile"), []byte("name=curl\nversion=8.10.0\nrelease=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, err := rsyncd.NewServer([]rsyncd.Module{
		{Name: "contrib", Path: modDir},
	}, rsyncd.DontRestrict())
	if err != nil {
		t.Fatalf("rsyncd.NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go func() {
		_ = srv.Serve(srvCtx, ln)
	}()

	dest := filepath.Join(root, "ports", "contrib")
	// Plant a stale file to verify --delete removes files absent upstream.
	if err := os.MkdirAll(filepath.Join(dest, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "stale", "old.txt"), []byte("remove me"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := sources.Source{
		Protocol:   sources.ProtoRsync,
		Collection: "contrib",
		URL:        fmt.Sprintf("rsync://%s/contrib/", ln.Addr().String()),
		DestDir:    dest,
	}

	out, err := syncRsync(context.Background(), src)
	if err != nil {
		t.Fatalf("pure-Go syncRsync failed: %v", err)
	}
	if out.Detail == "" {
		t.Fatalf("expected non-empty rsync detail")
	}

	got, err := os.ReadFile(filepath.Join(dest, "curl", "Pkgfile"))
	if err != nil || !strings.Contains(string(got), "version=8.10.0") {
		t.Fatalf("synced curl/Pkgfile mismatch: %q, err=%v", string(got), err)
	}
	if _, err := os.Stat(filepath.Join(dest, "stale", "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale/old.txt to be deleted by rsync --delete")
	}
}

func TestPureGoTarballSync(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("name=zig\nversion=0.14.0\nrelease=1\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "ports-main/zig/Pkgfile",
		Mode: 0o644,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "rolling")
	src := sources.Source{
		Protocol:   sources.ProtoTarball,
		Collection: "rolling",
		URL:        srv.URL + "/ports.tar.gz",
		DestDir:    dest,
	}
	out, err := syncTarball(context.Background(), srv.Client(), src)
	if err != nil || out.Skipped {
		t.Fatalf("syncTarball failed: out=%+v err=%v", out, err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "zig", "Pkgfile"))
	if err != nil || !strings.Contains(string(got), "name=zig") {
		t.Fatalf("tarball extract mismatch: %q, err=%v", string(got), err)
	}
}

func TestBoundedWorkerPoolConcurrency(t *testing.T) {
	var inFlight int32
	var maxSeen int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/REPO") {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				prev := atomic.LoadInt32(&maxSeen)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxSeen, prev, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			_, _ = w.Write([]byte("d:p1\nf:" + md5Hex("pkg\n") + ":p1/Pkgfile\n"))
			return
		}
		_, _ = w.Write([]byte("pkg\n"))
	}))
	defer srv.Close()

	baseDir := t.TempDir()
	var srcs []sources.Source
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("col%d", i)
		srcs = append(srcs, sources.Source{
			Protocol:   sources.ProtoHTTPUp,
			Collection: name,
			URL:        fmt.Sprintf("%s/%s/", srv.URL, name),
			DestDir:    filepath.Join(baseDir, name),
		})
	}

	var stdout, stderr bytes.Buffer
	engine := NewEngine(2, NewPrinter(&stdout, &stderr))
	engine.HTTPClient = srv.Client()

	summary := engine.SyncAll(context.Background(), srcs)
	if summary.Failed != 0 || summary.Updated != 6 {
		t.Fatalf("unexpected summary: %+v\nstderr=%s", summary, stderr.String())
	}
	if peak := atomic.LoadInt32(&maxSeen); peak > 2 {
		t.Fatalf("worker pool exceeded bound: max concurrent = %d, want <= 2", peak)
	}
}

func TestLinkNestedCollectionsAndCountPorts(t *testing.T) {
	portsDir := t.TempDir()
	kryxDir := filepath.Join(portsDir, "kryx")
	for _, rel := range []string{
		"pkg-tools/kpm/Pkgfile",
		"pkg-tools/zig/Pkgfile",
		"kernel/linux6.18/Pkgfile",
		"docs/README.md",
	} {
		p := filepath.Join(kryxDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("name=test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := countPorts(kryxDir); got != 3 {
		t.Fatalf("countPorts(kryxDir) = %d, want 3", got)
	}

	linkNestedCollections(kryxDir)

	for _, sub := range []string{"pkg-tools", "kernel"} {
		target, err := os.Readlink(filepath.Join(portsDir, sub))
		if err != nil {
			t.Fatalf("expected symlink %s: %v", sub, err)
		}
		want := filepath.Join("kryx", sub)
		if target != want {
			t.Fatalf("symlink %s -> %q, want %q", sub, target, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(portsDir, "docs")); !os.IsNotExist(err) {
		t.Fatalf("non-collection directory 'docs' should not be symlinked")
	}
}
