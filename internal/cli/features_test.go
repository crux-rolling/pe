package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryx-linux/kpm/internal/repo"
	"github.com/kryx-linux/kpm/internal/signing"
	"github.com/kryx-linux/kpm/internal/state"
)

func TestAdvancedFeaturesWorldInfoReverseDepsAutoremoveHoldDowngradeClean(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	cacheDir := filepath.Join(tmp, "cache")
	binServeDir := filepath.Join(tmp, "serve")
	if err := os.MkdirAll(binServeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build binary archives: libbase (1.0-1 and 2.0-1) and app (1.0-1 depending on libbase).
	libV1 := buildTestArchive(t, binServeDir, "libbase", "1.0", "1", map[string]string{
		"usr/lib/libbase.txt": "v1\n",
	})
	libV2 := buildTestArchive(t, binServeDir, "libbase", "2.0", "1", map[string]string{
		"usr/lib/libbase.txt": "v2\n",
	})
	appV1 := buildTestArchive(t, binServeDir, "app", "1.0", "1", map[string]string{
		"usr/bin/app": "#!/bin/sh\necho app\n",
	})

	sumLibV1, _ := repo.SHA256(libV1)
	sumLibV2, _ := repo.SHA256(libV2)
	sumAppV1, _ := repo.SHA256(appV1)

	// Generate signify keypair and serve index.json + index.json.sig over httptest.
	pubData, secData, _, err := signing.GenerateSignifyKeyPair("test-repo")
	if err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(tmp, "repo.pub")
	secPath := filepath.Join(tmp, "repo.sec")
	_ = os.WriteFile(pubPath, pubData, 0o644)
	_ = os.WriteFile(secPath, secData, 0o600)

	var srvURL string
	tamperSig := false
	buildIndex := func(includeLibV2 bool) ([]byte, []byte) {
		idx := repo.New()
		repo.Merge(&idx,
			repo.Entry{
				Pkgname: "libbase", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64",
				Filename: filepath.Base(libV1), URL: srvURL + "/files/" + url.PathEscape(filepath.Base(libV1)),
				SHA256: sumLibV1, Description: "Base library", Collection: "core",
			},
			repo.Entry{
				Pkgname: "app", Pkgver: "1.0", Pkgrel: "1", Arch: "x86_64",
				Filename: filepath.Base(appV1), URL: srvURL + "/files/" + url.PathEscape(filepath.Base(appV1)),
				SHA256: sumAppV1, Description: "Demo app", Collection: "opt",
				Depends: []string{"libbase"}, MakeDepends: []string{"cmake"},
			},
		)
		if includeLibV2 {
			repo.Merge(&idx, repo.Entry{
				Pkgname: "libbase", Pkgver: "2.0", Pkgrel: "1", Arch: "x86_64",
				Filename: filepath.Base(libV2), URL: srvURL + "/files/" + url.PathEscape(filepath.Base(libV2)),
				SHA256: sumLibV2, Description: "Base library", Collection: "core",
			})
		}
		raw, _ := json.MarshalIndent(idx, "", "  ")
		raw = append(raw, '\n')
		priv, kn, _, _ := signing.ParsePrivateKeyWithKeyNum(secData)
		sig, _ := signing.SignBytesSignify(priv, kn, "test sig", raw)
		if tamperSig {
			sig = []byte("untrusted comment: bad\n" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
		}
		return raw, sig
	}

	includeV2 := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.json":
			raw, _ := buildIndex(includeV2)
			_, _ = w.Write(raw)
		case r.URL.Path == "/index.json.sig":
			_, sig := buildIndex(includeV2)
			_, _ = w.Write(sig)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			name := strings.TrimPrefix(r.URL.Path, "/files/")
			http.ServeFile(w, r, filepath.Join(binServeDir, name))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	cfgPath := filepath.Join(tmp, "kpm.ini")
	cfgBody := fmt.Sprintf(`[paths]
root = %s
cache_dir = %s
source_cache = %s/sources
package_dir = %s/packages
work_dir = %s/work
keys_dir = %s/keys.d

[repo]
repository = test/ports
tag = rolling-x86_64
arch = x86_64
index_url = %s/index.json
verify_index_signature = true
pubkey = %s
timeout_seconds = 30

[ui]
color = never
`, root, cacheDir, cacheDir, cacheDir, cacheDir, tmp, srvURL, pubPath)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}
	runKPM := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		fullArgs := append([]string{"--config", cfgPath}, args...)
		c := RunWithOptions(fullArgs, &out, &errOut, rt)
		return c, out.String(), errOut.String()
	}

	// 1. Verified signify sync succeeds.
	if code, out, errOut := runKPM("sync"); code != 0 || !strings.Contains(out, "verified") {
		t.Fatalf("kpm sync verify failed (%d): out=%q err=%q", code, out, errOut)
	}

	// 2. Tampered signature MUST fail AND preserve existing cached index.json!
	cachedBefore, _ := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	tamperSig = true
	if code, _, errOut := runKPM("sync"); code == 0 || !strings.Contains(errOut, "signature verification failed") {
		t.Fatalf("expected tampered sync to fail, got code=%d err=%q", code, errOut)
	}
	cachedAfter, _ := os.ReadFile(filepath.Join(cacheDir, "index.json"))
	if !bytes.Equal(cachedBefore, cachedAfter) {
		t.Fatalf("tampered sync modified or deleted cached index.json!")
	}
	tamperSig = false

	// 3. Install app with --deps: app goes to world, libbase does NOT.
	if code, out, errOut := runKPM("install", "--deps", "app"); code != 0 {
		t.Fatalf("kpm install --deps app failed (%d): out=%q err=%q", code, out, errOut)
	}
	world, _ := state.LoadWorld(root)
	if !world["app"] || world["libbase"] {
		t.Fatalf("expected world={app}, got %v", world)
	}

	// 4. kpm info libbase shows Required By: app
	if code, out, errOut := runKPM("info", "libbase"); code != 0 || !strings.Contains(out, "Required By:") || !strings.Contains(out, "app") {
		t.Fatalf("kpm info libbase = (%d) out=%q err=%q", code, out, errOut)
	}
	if code, out, _ := runKPM("info", "app"); code != 0 || !strings.Contains(out, "Make Depends:") || !strings.Contains(out, "cmake") {
		t.Fatalf("kpm info app missing Make Depends: out=%q", out)
	}

	// 5. Reverse dependency check blocks removing libbase without --force
	if code, _, errOut := runKPM("remove", "libbase"); code == 0 || !strings.Contains(errOut, "app") {
		t.Fatalf("expected remove libbase to abort due to reverse dep app, got code=%d err=%q", code, errOut)
	}

	// 6. Publish libbase 2.0-1; hold libbase; kpm update MUST skip it!
	includeV2 = true
	if code, out, errOut := runKPM("hold", "libbase"); code != 0 || !strings.Contains(out, "held") {
		t.Fatalf("kpm hold libbase failed (%d): out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKPM("update"); code != 0 || !strings.Contains(out, "skipping held package libbase") {
		t.Fatalf("expected kpm update to skip held libbase, got (%d) out=%q err=%q", code, out, errOut)
	}

	// 7. Unhold libbase; kpm update upgrades libbase 1.0-1 -> 2.0-1
	if code, _, errOut := runKPM("unhold", "libbase"); code != 0 {
		t.Fatalf("kpm unhold failed (%d): %s", code, errOut)
	}
	if code, out, errOut := runKPM("update"); code != 0 || !strings.Contains(out, "1.0-1 -> 2.0-1") {
		t.Fatalf("expected kpm update to upgrade libbase, got (%d) out=%q err=%q", code, out, errOut)
	}

	// 8. kpm downgrade libbase rolls back 2.0-1 -> 1.0-1
	if code, out, errOut := runKPM("downgrade", "libbase"); code != 0 || !strings.Contains(out, "2.0-1 -> 1.0-1") {
		t.Fatalf("kpm downgrade libbase = (%d) out=%q err=%q", code, out, errOut)
	}

	// 9. kpm install libbase=2.0-1 installs pinned version explicitly
	if code, out, errOut := runKPM("install", "libbase=2.0-1"); code != 0 || !strings.Contains(out, "2.0-1") {
		t.Fatalf("kpm install libbase=2.0-1 = (%d) out=%q err=%q", code, out, errOut)
	}
	// Remove libbase from world so we can test autoremove cleanly after removing app
	_ = state.RemoveFromWorld(root, "libbase")

	// 10. Remove app -> libbase becomes an orphan -> autoremove --dry-run lists it, autoremove deletes it
	if code, _, errOut := runKPM("remove", "app"); code != 0 {
		t.Fatalf("kpm remove app failed (%d): %s", code, errOut)
	}
	// Keep one dummy package in world so world is non-empty when checking orphans
	_ = state.AddToWorld(root, "keep-sentinel")
	if code, out, errOut := runKPM("autoremove", "--dry-run"); code != 0 || !strings.Contains(out, "libbase") {
		t.Fatalf("kpm autoremove --dry-run = (%d) out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := runKPM("autoremove"); code != 0 || !strings.Contains(out, "libbase") {
		t.Fatalf("kpm autoremove = (%d) out=%q err=%q", code, out, errOut)
	}

	// 11. kpm clean removes partial & stale cached archives
	partialFile := filepath.Join(cacheDir, "bin", "stale.pkg.tar.zst.partial")
	_ = os.WriteFile(partialFile, []byte("junk"), 0o644)
	if code, out, errOut := runKPM("clean", "--all"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("kpm clean --all = (%d) out=%q err=%q", code, out, errOut)
	}
	if _, err := os.Stat(partialFile); !os.IsNotExist(err) {
		t.Fatalf("expected partial file removed by kpm clean")
	}
}

func TestCheckFilesAndBrokenELF(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	pkgs := filepath.Join(tmp, "pkgs")
	_ = os.MkdirAll(pkgs, 0o755)

	// Build a minimal package and install it.
	pkgPath := buildTestArchive(t, pkgs, "checkme", "1.0", "1", map[string]string{
		"usr/bin/checkme": "#!/bin/sh\necho ok\n",
		"etc/checkme.cfg": "k=v\n",
	})
	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}
	var out, errOut bytes.Buffer
	if code := RunWithOptions([]string{"--root", root, "install", pkgPath}, &out, &errOut, rt); code != 0 {
		t.Fatalf("install failed: %s", errOut.String())
	}

	// 1. kpm check --files passes on intact package
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--root", root, "check", "--files"}, &out, &errOut, rt); code != 0 || !strings.Contains(out.String(), "all files intact") {
		t.Fatalf("check --files intact failed (%d): out=%q err=%q", code, out.String(), errOut.String())
	}

	// 2. Delete a tracked file -> kpm check --files fails and reports missing file
	_ = os.Remove(filepath.Join(root, "etc", "checkme.cfg"))
	out.Reset()
	errOut.Reset()
	if code := RunWithOptions([]string{"--root", root, "check", "--files"}, &out, &errOut, rt); code == 0 || !strings.Contains(errOut.String(), "etc/checkme.cfg") {
		t.Fatalf("expected check --files failure on missing file, got (%d) out=%q err=%q", code, out.String(), errOut.String())
	}

	// 3. Test kpm check --broken using a real dynamically-linked ELF binary from the host (e.g. /bin/sh)
	// placed into an isolated root that lacks libc.so.6 so DT_NEEDED is guaranteed unresolved!
	shPath, err := exec.LookPath("sh")
	if err == nil {
		if realSh, rerr := filepath.EvalSymlinks(shPath); rerr == nil {
			shPath = realSh
		}
		if data, rerr := os.ReadFile(shPath); rerr == nil && len(data) > 4 && data[0] == 0x7f && data[1] == 'E' {
			elfRoot := filepath.Join(tmp, "elfroot")
			elfStage := t.TempDir()
			_ = os.MkdirAll(filepath.Join(elfStage, "usr", "bin"), 0o755)
			_ = os.WriteFile(filepath.Join(elfStage, "usr", "bin", "testelf"), data, 0o755)
			elfPkg := filepath.Join(pkgs, "testelf#1.0-1.pkg.tar.gz")
			if err := buildArchiveFromDir(elfStage, elfPkg); err == nil {
				out.Reset()
				errOut.Reset()
				if code := RunWithOptions([]string{"--root", elfRoot, "install", elfPkg}, &out, &errOut, rt); code == 0 {
					out.Reset()
					errOut.Reset()
					code = RunWithOptions([]string{"--root", elfRoot, "check", "--broken"}, &out, &errOut, rt)
					// Dynamically linked sh in an empty root has missing .so deps (unless host sh is static).
					if strings.Contains(errOut.String(), "BROKEN") {
						if code == 0 || !strings.Contains(errOut.String(), "testelf") {
							t.Fatalf("expected non-zero exit mentioning testelf, got code=%d err=%q", code, errOut.String())
						}
					}
				}
			}
		}
	}
}

func buildArchiveFromDir(srcDir, dstPath string) error {
	cmd := exec.Command("tar", "-czf", dstPath, "-C", srcDir, ".")
	return cmd.Run()
}
