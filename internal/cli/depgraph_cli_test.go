package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryx-linux/kpm/internal/pkgdb"
	"github.com/kryx-linux/kpm/internal/repo"
	"github.com/kryx-linux/kpm/internal/state"
)

func writeDepgraphTestConfig(t *testing.T, dir, root, cacheDir, portsDir, indexURL string) string {
	t.Helper()
	if err := pkgdb.EnsureInitialized(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "kpm.ini")
	content := fmt.Sprintf(`[paths]
root = %s
ports_dir = %s
cache_dir = %s
source_cache = %s
package_dir = %s
work_dir = %s

[repo]
arch = x86_64
index_url = %s
verify_signature = false
`, root, portsDir, cacheDir, filepath.Join(cacheDir, "sources"), filepath.Join(cacheDir, "packages"), filepath.Join(cacheDir, "work"), indexURL)
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestUpdateTopologicalOrderThroughUnchangedDep(t *testing.T) {
	// Regression test: "app" < "libbase" alphabetically, and "app" depends on
	// unchanged "midlib" which depends on "libbase", plus "app" adds a brand new
	// dependency "newdep" depending on "libbase".
	// Exact upgrade/install order MUST be: libbase -> newdep -> app.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	cacheDir := filepath.Join(tmp, "cache")
	remoteDir := filepath.Join(tmp, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build v1 archives for initial seed install.
	pLib1, _, _ := createPkgArchive(t, tmp, "libbase", "1.0-1", map[string]string{"usr/lib/libbase.so": "v1"})
	pMid1, sumMid1, sizeMid1 := createPkgArchive(t, remoteDir, "midlib", "1.0-1", map[string]string{"usr/lib/midlib.so": "v1"})
	pApp1, _, _ := createPkgArchive(t, tmp, "app", "1.0-1", map[string]string{"usr/bin/app": "v1"})

	// Build v2 of libbase, app, and v1 of newdep served via httptest.
	pLib2, sumLib2, sizeLib2 := createPkgArchive(t, remoteDir, "libbase", "2.0-1", map[string]string{"usr/lib/libbase.so": "v2"})
	pNew1, sumNew1, sizeNew1 := createPkgArchive(t, remoteDir, "newdep", "1.0-1", map[string]string{"usr/lib/newdep.so": "v1"})
	pApp2, sumApp2, sizeApp2 := createPkgArchive(t, remoteDir, "app", "2.0-1", map[string]string{"usr/bin/app": "v2"})

	srv := httptest.NewServer(http.FileServer(http.Dir(remoteDir)))
	defer srv.Close()

	idx := repo.New()
	repo.Merge(&idx,
		repo.Entry{Pkgname: "libbase", Pkgver: "2.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pLib2), URL: srv.URL + "/" + url.PathEscape(filepath.Base(pLib2)), SHA256: sumLib2, Size: sizeLib2},
		repo.Entry{Pkgname: "midlib", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pMid1), URL: srv.URL + "/" + url.PathEscape(filepath.Base(pMid1)), SHA256: sumMid1, Size: sizeMid1, Depends: []string{"libbase"}},
		repo.Entry{Pkgname: "newdep", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pNew1), URL: srv.URL + "/" + url.PathEscape(filepath.Base(pNew1)), SHA256: sumNew1, Size: sizeNew1, Depends: []string{"libbase"}},
		repo.Entry{Pkgname: "app", Pkgver: "2.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pApp2), URL: srv.URL + "/" + url.PathEscape(filepath.Base(pApp2)), SHA256: sumApp2, Size: sizeApp2, Depends: []string{"midlib", "newdep"}},
	)
	if err := repo.Save(filepath.Join(cacheDir, "index.json"), idx); err != nil {
		t.Fatal(err)
	}

	cfgPath := writeDepgraphTestConfig(t, tmp, root, cacheDir, filepath.Join(tmp, "ports"), srv.URL+"/index.json")
	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}

	var out, errOut bytes.Buffer
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "install", pLib1, pMid1, pApp1}, &out, &errOut, rt); code != 0 {
		t.Fatalf("seed install failed (%d): %s", code, errOut.String())
	}

	// 1. Dry-run must show libbase -> newdep -> app!
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "update", "--no-sync", "--dry-run"}, &out, &errOut, rt); code != 0 {
		t.Fatalf("update --dry-run failed (%d): %s", code, errOut.String())
	}
	dryStr := out.String()
	iLib := strings.Index(dryStr, "libbase 1.0-1 -> 2.0-1")
	iNew := strings.Index(dryStr, "newdep (new dependency) 1.0-1")
	iApp := strings.Index(dryStr, "app 1.0-1 -> 2.0-1")
	if iLib < 0 || iNew < 0 || iApp < 0 || !(iLib < iNew && iNew < iApp) {
		t.Fatalf("dry-run order wrong (lib=%d new=%d app=%d):\n%s", iLib, iNew, iApp, dryStr)
	}

	// 2. Real update must execute libbase -> newdep -> app!
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "update", "--no-sync"}, &out, &errOut, rt); code != 0 {
		t.Fatalf("update failed (%d): %s", code, errOut.String())
	}
	upStr := out.String()
	uLib := strings.Index(upStr, "updated libbase 1.0-1 -> 2.0-1")
	uNew := strings.Index(upStr, "installed dependency newdep 1.0-1")
	uApp := strings.Index(upStr, "updated app 1.0-1 -> 2.0-1")
	if uLib < 0 || uNew < 0 || uApp < 0 || !(uLib < uNew && uNew < uApp) {
		t.Fatalf("real update order wrong (lib=%d new=%d app=%d):\n%s", uLib, uNew, uApp, upStr)
	}
}

func TestRemoveReverseOrderCascadeAutoremoveDepsAndWhy(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	cacheDir := filepath.Join(tmp, "cache")

	pLib, _, _ := createPkgArchive(t, tmp, "aaa-lib", "1.0-1", map[string]string{"usr/lib/liba.so": "1"})
	pMid, _, _ := createPkgArchive(t, tmp, "mmm-mid", "1.0-1", map[string]string{"usr/lib/libm.so": "1"})
	pApp, _, _ := createPkgArchive(t, tmp, "zzz-app", "1.0-1", map[string]string{"usr/bin/zzz": "1"})
	pKeep, _, _ := createPkgArchive(t, tmp, "keep-pkg", "1.0-1", map[string]string{"usr/bin/keep": "1"})

	idx := repo.New()
	repo.Merge(&idx,
		repo.Entry{Pkgname: "aaa-lib", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pLib)},
		repo.Entry{Pkgname: "mmm-mid", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pMid), Depends: []string{"aaa-lib"}},
		repo.Entry{Pkgname: "zzz-app", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pApp), Depends: []string{"mmm-mid"}},
		repo.Entry{Pkgname: "keep-pkg", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64", Filename: filepath.Base(pKeep)},
	)
	if err := repo.Save(filepath.Join(cacheDir, "index.json"), idx); err != nil {
		t.Fatal(err)
	}

	cfgPath := writeDepgraphTestConfig(t, tmp, root, cacheDir, filepath.Join(tmp, "ports"), "https://example.invalid/index.json")
	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}

	var out, errOut bytes.Buffer
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "install", pLib, pMid, pApp, pKeep}, &out, &errOut, rt); code != 0 {
		t.Fatalf("install failed (%d): %s", code, errOut.String())
	}

	// Mark only zzz-app and keep-pkg in world so aaa-lib and mmm-mid are dependencies.
	_ = state.RemoveFromWorld(root, "aaa-lib", "mmm-mid")

	// 1. Test `kpm why aaa-lib` -> zzz-app -> mmm-mid -> aaa-lib
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "why", "aaa-lib"}, &out, &errOut, rt); code != 0 {
		t.Fatalf("why failed (%d): %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "zzz-app -> mmm-mid -> aaa-lib") {
		t.Fatalf("expected dependency chain in why output:\n%s", out.String())
	}

	// 2. Test `kpm deps --tree zzz-app`, `--waves`, `--dot`, `--cycles`
	for _, flagArg := range [][]string{
		{"deps", "--tree", "zzz-app"},
		{"deps", "--waves", "zzz-app"},
		{"deps", "--dot", "zzz-app"},
		{"deps", "--cycles"},
	} {
		out.Reset()
		errOut.Reset()
		args := append([]string{"--no-color", "-c", cfgPath}, flagArg...)
		if code := RunWithOptions(args, &out, &errOut, rt); code != 0 {
			t.Fatalf("%v failed (%d): %s", flagArg, code, errOut.String())
		}
		if strings.TrimSpace(out.String()) == "" {
			t.Fatalf("%v produced empty output", flagArg)
		}
	}

	// 3. Test `kpm remove --cascade --dry-run aaa-lib` -> zzz-app, mmm-mid, aaa-lib
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "remove", "--cascade", "--dry-run", "aaa-lib"}, &out, &errOut, rt); code != 0 {
		t.Fatalf("remove --cascade --dry-run failed (%d): %s", code, errOut.String())
	}
	dryRem := out.String()
	iz := strings.Index(dryRem, "zzz-app")
	im := strings.Index(dryRem, "mmm-mid")
	ia := strings.Index(dryRem, "aaa-lib")
	if !(iz >= 0 && im > iz && ia > im) {
		t.Fatalf("cascade removal order wrong:\n%s", dryRem)
	}

	// 4. Remove zzz-app from world and run autoremove: MUST remove zzz-app -> mmm-mid -> aaa-lib (NOT alphabetical!)
	_ = state.RemoveFromWorld(root, "zzz-app")
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--no-color", "-c", cfgPath, "autoremove"}, &out, &errOut, rt); code != 0 {
		t.Fatalf("autoremove failed (%d): %s", code, errOut.String())
	}
	autoStr := out.String()
	rz := strings.Index(autoStr, "removed orphan zzz-app")
	rm := strings.Index(autoStr, "removed orphan mmm-mid")
	ra := strings.Index(autoStr, "removed orphan aaa-lib")
	if !(rz >= 0 && rm > rz && ra > rm) {
		t.Fatalf("autoremove order wrong (must be reverse-dep, not alphabetical):\n%s", autoStr)
	}
}
