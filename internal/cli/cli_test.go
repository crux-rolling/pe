package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ezke5/pe/internal/archive"
	"github.com/ezke5/pe/internal/pkgdb"
	"github.com/ezke5/pe/internal/repo"
)

func createPkgArchive(t *testing.T, dir, name, verRel string, files map[string]string) (string, string, int64) {
	t.Helper()
	stage := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(stage, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outPath := filepath.Join(dir, name+"#"+verRel+".pkg.tar.zst")
	if err := archive.Create(stage, outPath, "zst"); err != nil {
		t.Fatal(err)
	}
	sum, err := repo.SHA256(outPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return outPath, sum, info.Size()
}

func TestEndToEndCLIWorkflow(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := pkgdb.EnsureInitialized(root); err != nil {
		t.Fatal(err)
	}

	// Create a local ports tree (left in place) with two ports:
	// - "demo" (also has a binary in index.json)
	// - "sourceonly" (NO binary in index.json -> `pe install` must refuse and instruct `pe build sourceonly`)
	portsDir := filepath.Join(base, "ports")
	for name, desc := range map[string]string{
		"demo":       "Demo binary package",
		"sourceonly": "Source only test port",
	} {
		dir := filepath.Join(portsDir, "opt", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		pkgfile := "# Description: " + desc + "\n" +
			"name=" + name + "\n" +
			"version=1.0\n" +
			"release=1\n" +
			"source=()\n" +
			"build() {\n" +
			"  install -D -m 0755 /dev/null \"$PKG/usr/bin/" + name + "\"\n" +
			"  printf '#!/bin/sh\\necho " + name + "\\n' > \"$PKG/usr/bin/" + name + "\"\n" +
			"}\n"
		if err := os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(pkgfile), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Build two versions of "demo" binary archives served via httptest.
	repoPkgsDir := filepath.Join(base, "remote_pkgs")
	if err := os.MkdirAll(repoPkgsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	v1Path, v1Sum, v1Size := createPkgArchive(t, repoPkgsDir, "demo", "1.0-1", map[string]string{
		"usr/bin/demo": "v1\n",
	})
	v2Path, v2Sum, v2Size := createPkgArchive(t, repoPkgsDir, "demo", "2.0-1", map[string]string{
		"usr/bin/demo": "v2\n",
	})

	serveV2 := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			idx := repo.New()
			repo.Merge(&idx, repo.Entry{
				Pkgname:     "demo",
				Pkgver:      "1.0",
				Pkgrel:      "1",
				Arch:        "x86_64",
				Filename:    filepath.Base(v1Path),
				URL:         srv.URL + "/demo-v1.pkg.tar.zst",
				SHA256:      v1Sum,
				Size:        v1Size,
				Description: "Demo binary package",
			})
			if serveV2 {
				repo.Merge(&idx, repo.Entry{
					Pkgname:     "demo",
					Pkgver:      "2.0",
					Pkgrel:      "1",
					Arch:        "x86_64",
					Filename:    filepath.Base(v2Path),
					URL:         srv.URL + "/demo-v2.pkg.tar.zst",
					SHA256:      v2Sum,
					Size:        v2Size,
					Description: "Demo binary package",
				})
			}
			_ = json.NewEncoder(w).Encode(idx)
		case "/demo-v1.pkg.tar.zst":
			http.ServeFile(w, r, v1Path)
		case "/demo-v2.pkg.tar.zst":
			http.ServeFile(w, r, v2Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Write manual pe.ini
	cfgPath := filepath.Join(base, "pe.ini")
	iniContent := `
[paths]
root = ` + root + `
ports_dir = ` + portsDir + `
collections = opt
cache_dir = ` + filepath.Join(base, "cache") + `
source_cache = ` + filepath.Join(base, "sources") + `
package_dir = ` + filepath.Join(base, "packages") + `
work_dir = ` + filepath.Join(base, "work") + `

[repo]
repository = ezke5/packages
tag = rolling-x86_64
arch = x86_64
index_url = ` + srv.URL + `/index.json

[build]
jobs = 2
cflags = -O2 -pipe
compression = zst
ignore_md5sum = true
ignore_signature = true
`
	if err := os.WriteFile(cfgPath, []byte(iniContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		var fullArgs []string
		// Allow test callers to pass global flags before subcommands cleanly.
		fullArgs = append(fullArgs, "--config", cfgPath)
		fullArgs = append(fullArgs, args...)
		code := RunWithOptions(fullArgs, &out, &errOut, rt)
		return code, out.String(), errOut.String()
	}

	// 1. pe sync
	if code, out, errOut := run("sync"); code != 0 || !strings.Contains(out, "synced") {
		t.Fatalf("sync failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 2. pe list --ports
	if code, out, errOut := run("list", "--ports"); code != 0 || !strings.Contains(out, "opt/demo") || !strings.Contains(out, "opt/sourceonly") {
		t.Fatalf("list --ports failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 3. pe search (short-form output, plain and colored modes)
	if code, out, errOut := run("search", "demo"); code != 0 || !strings.Contains(out, "demo 1.0-1 [bin,port] - Demo binary package") {
		t.Fatalf("search failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := run("--color=always", "search", "demo"); code != 0 || !strings.Contains(out, "\x1b[") || !strings.Contains(out, "demo") {
		t.Fatalf("colored search failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := run("--no-color", "search", "demo"); code != 0 || strings.Contains(out, "\x1b[") {
		t.Fatalf("no-color search failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 4. pe install demo (binary-first, with download progress log)
	if code, out, errOut := run("install", "demo"); code != 0 || !strings.Contains(out, "installed demo 1.0-1") || !strings.Contains(out, "[sha256 ok]") {
		t.Fatalf("install demo failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 4b. Verify flags work AFTER package names (e.g. `pe install demo --upgrade --force`)
	if code, out, errOut := run("install", "demo", "--upgrade", "--force"); code != 0 || !strings.Contains(out, "installed demo 1.0-1") {
		t.Fatalf("interspersed flags install failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 4c. Verify --reinstall / --reins / -R and short subcommand aliases (`in`, `i`, `s`, `ls`)
	if code, out, errOut := run("in", "--reins", "demo"); code != 0 || !strings.Contains(out, "installed demo 1.0-1") {
		t.Fatalf("in --reins demo failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := run("i", "demo", "-R"); code != 0 || !strings.Contains(out, "installed demo 1.0-1") {
		t.Fatalf("i demo -R failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := run("s", "-b", "demo"); code != 0 || !strings.Contains(out, "demo 1.0-1") {
		t.Fatalf("s -b demo failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := run("ls", "-p"); code != 0 || !strings.Contains(out, "opt/demo") {
		t.Fatalf("ls -p failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 5. pe install sourceonly MUST fail and instruct `pe build sourceonly` (never auto-build!)
	code, out, errOut := run("install", "sourceonly")
	if code == 0 {
		t.Fatalf("expected install sourceonly to fail when binary is unavailable; out=%q", out)
	}
	if !strings.Contains(errOut, "pe build sourceonly") {
		t.Fatalf("expected instruction to run 'pe build sourceonly', got stderr=%q", errOut)
	}
	// Verify no archive was auto-built in packages/
	if matches, _ := filepath.Glob(filepath.Join(base, "packages", "sourceonly*")); len(matches) != 0 {
		t.Fatalf("install must not auto-build sourceonly, found %v", matches)
	}

	// 6. pe build --install sourceonly (explicit build from ports)
	if code, out, errOut := run("build", "--install", "sourceonly"); code != 0 || !strings.Contains(out, "installed sourceonly 1.0-1") {
		t.Fatalf("build --install sourceonly failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 7. pe update upgrades demo from 1.0-1 -> 2.0-1
	serveV2 = true
	if code, out, errOut := run("update"); code != 0 || !strings.Contains(out, "updated demo 1.0-1 -> 2.0-1") {
		t.Fatalf("update failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 8. pe list (installed)
	if code, out, errOut := run("list"); code != 0 || !strings.Contains(out, "demo 2.0-1") || !strings.Contains(out, "sourceonly 1.0-1") {
		t.Fatalf("list installed failed: code=%d out=%q err=%q", code, out, errOut)
	}

	// 9. pe remove / pe uninstall
	if code, out, errOut := run("remove", "demo"); code != 0 || !strings.Contains(out, "removed demo") {
		t.Fatalf("remove demo failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if code, out, errOut := run("uninstall", "sourceonly"); code != 0 || !strings.Contains(out, "removed sourceonly") {
		t.Fatalf("uninstall sourceonly failed: code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestBuildDepsAutoInstallSkipAndBinaryReuse(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := pkgdb.EnsureInitialized(root); err != nil {
		t.Fatal(err)
	}
	portsDir := filepath.Join(base, "ports")
	toolDir := filepath.Join(portsDir, "opt", "mytool")
	appDir := filepath.Join(portsDir, "opt", "myapp")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// mytool installs a helper script at /usr/bin/mytool
	toolPkgfile := `# Description: Build-time helper tool
name=mytool
version=1.0
release=1
source=()
build() {
	mkdir -p "$PKG/usr/bin"
	printf '#!/bin/sh\necho built-by-mytool\n' > "$PKG/usr/bin/mytool"
	chmod 0755 "$PKG/usr/bin/mytool"
}
`
	if err := os.WriteFile(filepath.Join(toolDir, "Pkgfile"), []byte(toolPkgfile), 0o644); err != nil {
		t.Fatal(err)
	}

	// myapp depends on mytool and executes <root>/usr/bin/mytool during build()
	appPkgfile := "# Description: App requiring mytool at build time\n" +
		"# Depends on: mytool\n" +
		"name=myapp\n" +
		"version=1.0\n" +
		"release=1\n" +
		"source=()\n" +
		"build() {\n" +
		"  OUT=$(" + filepath.Join(root, "usr/bin/mytool") + ")\n" +
		"  test \"$OUT\" = \"built-by-mytool\"\n" +
		"  mkdir -p \"$PKG/usr/bin\"\n" +
		"  printf '%s\\n' \"$OUT\" > \"$PKG/usr/bin/myapp\"\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(appDir, "Pkgfile"), []byte(appPkgfile), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(base, "pe.ini")
	iniContent := `
[paths]
root = ` + root + `
ports_dir = ` + portsDir + `
collections = opt
cache_dir = ` + filepath.Join(base, "cache") + `
source_cache = ` + filepath.Join(base, "sources") + `
package_dir = ` + filepath.Join(base, "packages") + `
work_dir = ` + filepath.Join(base, "work") + `

[build]
jobs = 2
compression = zst
ignore_md5sum = true
ignore_signature = true
`
	if err := os.WriteFile(cfgPath, []byte(iniContent), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		fullArgs := append([]string{"--config", cfgPath}, args...)
		code := RunWithOptions(fullArgs, &out, &errOut, rt)
		return code, out.String(), errOut.String()
	}

	// 1. `pe b -f -d myapp` must build & auto-install mytool, then succeed building myapp
	//    without auto-installing myapp itself.
	code, out, errOut := run("b", "-f", "-d", "myapp")
	if code != 0 {
		t.Fatalf("pe b -f -d myapp failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "installed dependency mytool 1.0-1") {
		t.Fatalf("expected mytool dependency to be auto-installed, got out=%q", out)
	}
	db, err := pkgdb.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if !db.FindPkg("mytool") {
		t.Fatalf("expected mytool in DB after build --deps")
	}
	if db.FindPkg("myapp") {
		t.Fatalf("expected target myapp NOT installed without --install")
	}

	// 2. Re-running `pe b -f -d myapp` skips already-installed mytool and only rebuilds myapp.
	code, out, errOut = run("b", "-f", "-d", "myapp")
	if code != 0 {
		t.Fatalf("second build --deps failed: code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "dependency mytool already installed, skipping") {
		t.Fatalf("expected already-installed mytool to be skipped, got out=%q", out)
	}

	// 3. Remove mytool and serve a binary mytool via httptest; verify `pe b -d myapp` uses binary dep,
	//    while `pe b -d -S myapp` builds dep from source.
	if code, _, errOut := run("r", "mytool"); code != 0 {
		t.Fatalf("pe r mytool failed: %q", errOut)
	}
	binPkgPath, binSum, binSize := createPkgArchive(t, filepath.Join(base, "remote"), "mytool", "1.0-1", map[string]string{
		"usr/bin/mytool": "#!/bin/sh\necho built-by-mytool\n",
	})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			idx := repo.New()
			repo.Merge(&idx, repo.Entry{
				Pkgname:  "mytool",
				Pkgver:   "1.0",
				Pkgrel:   "1",
				Arch:     "x86_64",
				Filename: filepath.Base(binPkgPath),
				URL:      srv.URL + "/mytool.pkg.tar.zst",
				SHA256:   binSum,
				Size:     binSize,
			})
			_ = json.NewEncoder(w).Encode(idx)
		case "/mytool.pkg.tar.zst":
			http.ServeFile(w, r, binPkgPath)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	iniWithRepo := iniContent + "\n[repo]\nindex_url = " + srv.URL + "/index.json\narch = x86_64\n"
	if err := os.WriteFile(cfgPath, []byte(iniWithRepo), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := run("sy"); code != 0 {
		t.Fatalf("pe sy failed: code=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = run("b", "-f", "-d", "myapp")
	if code != 0 || !strings.Contains(out, "installing binary dependency mytool 1.0-1") {
		t.Fatalf("expected binary dependency reuse, code=%d out=%q err=%q", code, out, errOut)
	}
}
