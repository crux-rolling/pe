package srcmachine

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/kryx-linux/kpm/internal/repo"
)

func TestPlanWithDepsFeaturesSkipAndIndexed(t *testing.T) {
	portsDir := t.TempDir()
	writePort := func(rel, body string) {
		t.Helper()
		p := filepath.Join(portsDir, filepath.FromSlash(rel), "Pkgfile")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePort("core/zlib", "name=zlib\nversion=1.4\nrelease=1\nbuild() { true; }\n")
	writePort("xorg/mesa", "# Depends on: zlib\nname=mesa\nversion=26.3\nrelease=1\nbuild() { true; }\n")
	writePort("xorg/xorg-xwayland", "# Depends on: mesa\nname=xorg-xwayland\nversion=24.1\nrelease=1\nbuild() { true; }\n")
	writePort("contrib/wlroots", "# Depends on: mesa\n# Optional: xorg-xwayland\nname=wlroots\nversion=0.20.2\nrelease=1\nbuild() { true; }\n")
	writePort("contrib/broken-32", "# Depends on: missing-32\nname=broken-32\nversion=1.0\nrelease=1\nbuild() { true; }\n")

	featPath := filepath.Join(t.TempDir(), "build-features.ini")
	if err := os.WriteFile(featPath, []byte("[wlroots]\nextra_deps = xorg-xwayland\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skipPath := filepath.Join(t.TempDir(), "skip-ports.txt")
	if err := os.WriteFile(skipPath, []byte("# quarantine\nbroken-32\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := LoadRegistry(portsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	features, err := LoadFeaturesFile(featPath)
	if err != nil {
		t.Fatal(err)
	}
	skipMap, err := LoadSkipFile(skipPath)
	if err != nil {
		t.Fatal(err)
	}

	// Expanding wlroots with --with-deps + features should pull zlib -> mesa -> xorg-xwayland -> wlroots across waves 0..3.
	plan, err := BuildMatrixPlanWithOptions(reg, PlanOptions{
		Targets:    []string{"wlroots"},
		MaxPorts:   50,
		ChunkSize:  1,
		WithDeps:   true,
		WaveFilter: -1,
		Features:   features,
		Skip:       skipMap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalPorts != 4 || plan.MaxWave != 3 {
		t.Fatalf("expected 4 ports across max_wave=3, got total=%d max_wave=%d jobs=%+v", plan.TotalPorts, plan.MaxWave, plan.Include)
	}
	if plan.Include[0].PkgNames[0] != "zlib" || plan.Include[0].Wave != 0 {
		t.Fatalf("wave 0 should be zlib, got %+v", plan.Include[0])
	}
	if plan.Include[3].PkgNames[0] != "wlroots" || plan.Include[3].Wave != 3 {
		t.Fatalf("wave 3 should be wlroots, got %+v", plan.Include[3])
	}

	// WaveFilter=2 should return only xorg-xwayland.
	w2, err := BuildMatrixPlanWithOptions(reg, PlanOptions{
		Targets:    []string{"wlroots"},
		MaxPorts:   50,
		ChunkSize:  1,
		WithDeps:   true,
		WaveFilter: 2,
		Features:   features,
		Skip:       skipMap,
	})
	if err != nil || w2.TotalPorts != 1 || w2.Include[0].PkgNames[0] != "xorg-xwayland" {
		t.Fatalf("wave 2 filter mismatch: %+v err=%v", w2, err)
	}

	// SkipIndexed should omit already-published zlib 1.4-1.
	idx := repo.New()
	repo.Merge(&idx, repo.Entry{Pkgname: "zlib", Pkgver: "1.4", Pkgrel: "1", Arch: "x86_64", Filename: "zlib#1.4-1.pkg.tar.zst"})
	inc, err := BuildMatrixPlanWithOptions(reg, PlanOptions{
		Targets:     []string{"wlroots"},
		MaxPorts:    50,
		ChunkSize:   1,
		WithDeps:    true,
		WaveFilter:  -1,
		Features:    features,
		Skip:        skipMap,
		SkipIndexed: &idx,
		Arch:        "x86_64",
	})
	if err != nil || inc.TotalPorts != 3 {
		t.Fatalf("expected 3 unindexed ports, got %+v err=%v", inc, err)
	}
}

func TestDiscoverRuntimeDepsShebangAndShardTag(t *testing.T) {
	// Build a minimal pkg.tar.gz containing an executable #!/bin/sh script and resolve via pkgdb.
	root := t.TempDir()
	dbDir := filepath.Join(root, "var", "lib", "pkg")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "db"), []byte("dash\n0.5.13-1\nbin/sh\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	arcPath := filepath.Join(t.TempDir(), "mytool#1.0-1.pkg.tar.gz")
	f, err := os.Create(arcPath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	script := []byte("#!/bin/sh\necho hi\n")
	if err := tw.WriteHeader(&tar.Header{Name: "usr/bin/mytool", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(script); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()

	deps, err := DiscoverRuntimeDeps(arcPath, root)
	if err != nil {
		t.Fatalf("DiscoverRuntimeDeps failed: %v", err)
	}
	if len(deps) != 1 || deps[0] != "dash" {
		t.Fatalf("expected [dash], got %v", deps)
	}

	if got := ShardTag("rolling-x86_64", "core", "glibc"); got != "rolling-x86_64-core" {
		t.Fatalf("unexpected shard tag: %q", got)
	}
	if got := ShardTag("rolling-x86_64", "contrib", "alacritty"); got != "rolling-x86_64-contrib-am" {
		t.Fatalf("unexpected contrib a-m shard tag: %q", got)
	}
	if got := ShardTag("rolling-x86_64", "contrib", "wlroots"); got != "rolling-x86_64-contrib-nz" {
		t.Fatalf("unexpected contrib n-z shard tag: %q", got)
	}
	if got := ShardTag("rolling-x86_64", "pkg-tools", "kpm"); got != "rolling-x86_64-kryx" {
		t.Fatalf("unexpected kryx shard tag: %q", got)
	}
}
