package srcmachine

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryx-linux/pe/internal/repo"
)

func TestRegistryMismatchLintBuildAndStage(t *testing.T) {
	tmp := t.TempDir()
	portsDir := filepath.Join(tmp, "ports")
	writePort := func(relDir, body string) {
		dir := filepath.Join(portsDir, filepath.FromSlash(relDir))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePort("core/helper", `# Description: Build helper
name=helper
version=1.0
release=1
source=()
build() {
	install -d "$PKG/usr/share/helper"
	echo ok > "$PKG/usr/share/helper/ready"
}
`)
	// Reproduces the exact kryx-linux/ports rolling/linux-6.18 -> name=kernel scenario
	writePort("rolling/linux-6.18", `# Description: Custom Linux 6.18 kernel
# Depends on: helper
name=kernel
version=6.18.64
release=2
source=()
build() {
	install -d "$PKG/boot"
	echo vmlinuz > "$PKG/boot/vmlinuz-6.18.64"
}
`)

	reg, err := LoadRegistry(portsDir, nil)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if len(reg.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(reg.Entries))
	}

	// All four lookup forms must resolve rolling/linux-6.18 (name=kernel)
	for _, q := range []string{"linux-6.18", "rolling/linux-6.18", "kernel", "rolling/kernel"} {
		e, ok := reg.Resolve(q)
		if !ok {
			t.Fatalf("Resolve(%q) returned not found", q)
		}
		if e.Port.Name != "kernel" || e.RelDir != "rolling/linux-6.18" {
			t.Fatalf("Resolve(%q) = %+v", q, e)
		}
	}

	// Lint should report 0 errors and 1 warning (dir basename mismatch)
	lr := Lint(reg, nil)
	if lr.ErrorCount() != 0 || lr.WarningCount() != 1 {
		t.Fatalf("unexpected lint report: errors=%d warnings=%d issues=%v", lr.ErrorCount(), lr.WarningCount(), lr.Issues)
	}

	// Build linux-6.18 with --deps into an isolated temp rootfs
	rootDir := filepath.Join(tmp, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootDir, "var", "lib", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "var", "lib", "pkg", "db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(tmp, "work")
	outDir := filepath.Join(tmp, "dist", "packages")

	var stdout, stderr bytes.Buffer
	res, err := Build(BuildOptions{
		PortsDir:        portsDir,
		Targets:         []string{"linux-6.18"},
		WorkRoot:        workRoot,
		OutDir:          outDir,
		Root:            rootDir,
		WithDeps:        true,
		SourceDeps:      true,
		Force:           true,
		IgnoreFootprint: true,
		IgnoreMD5Sum:    true,
		IgnoreSignature: true,
		NoStrip:         true,
		Compression:     "gz",
		SkipRootCheck:   true,
		SkipLdconfig:    true,
		ColorMode:       "never",
		Out:             &stdout,
		ErrOut:          &stderr,
	})
	if err != nil {
		t.Fatalf("Build failed: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	if len(res.BuiltArchives) != 2 {
		t.Fatalf("expected 2 built archives (helper + kernel), got %v", res.BuiltArchives)
	}

	// Stage archives + GitHub-safe upload copies + rewrite URLs
	stageDir := filepath.Join(tmp, "stage")
	uploadDir := filepath.Join(stageDir, "upload")
	indexPath := filepath.Join(stageDir, "index.json")

	stageRes, err := Stage(StageOptions{
		PackagesDir: outDir,
		PortsDir:    portsDir,
		IndexPath:   indexPath,
		UploadDir:   uploadDir,
		BaseURL:     "https://github.com/kryx-linux/ports/releases/download/rolling-x86_64",
		Arch:        "x86_64",
		ErrOut:      &stderr,
	})
	if err != nil {
		t.Fatalf("Stage failed: %v", err)
	}
	if stageRes.IndexedCount != 2 || len(stageRes.UploadFiles) != 2 {
		t.Fatalf("unexpected stage result: %+v", stageRes)
	}
	if !strings.HasSuffix(stageRes.UploadFiles[1], "kernel.6.18.64-2.pkg.tar.gz") &&
		!strings.HasSuffix(stageRes.UploadFiles[0], "kernel.6.18.64-2.pkg.tar.gz") {
		t.Fatalf("expected GitHub-safe kernel asset name, got %v", stageRes.UploadFiles)
	}

	assetsJSON := filepath.Join(stageDir, "assets.json")
	payload := `{"assets":[
		{"name":"helper.1.0-1.pkg.tar.gz","url":"https://github.com/kryx-linux/ports/releases/download/rolling-x86_64/helper.1.0-1.pkg.tar.gz"},
		{"name":"kernel.6.18.64-2.pkg.tar.gz","url":"https://github.com/kryx-linux/ports/releases/download/rolling-x86_64/kernel.6.18.64-2.pkg.tar.gz"}
	]}`
	if err := os.WriteFile(assetsJSON, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RewriteReleaseURLs(indexPath, assetsJSON, outDir); err != nil {
		t.Fatalf("RewriteReleaseURLs failed: %v", err)
	}

	idx, err := repo.Load(indexPath)
	if err != nil {
		t.Fatalf("repo.Load: %v", err)
	}
	kEntry, ok := repo.Find(idx, "kernel", "x86_64")
	if !ok {
		t.Fatalf("kernel entry missing from index.json")
	}
	if kEntry.Collection != "rolling" || kEntry.Description != "Custom Linux 6.18 kernel" {
		t.Fatalf("unexpected enriched metadata: %+v", kEntry)
	}
	if !strings.HasSuffix(kEntry.URL, "/kernel.6.18.64-2.pkg.tar.gz") {
		t.Fatalf("unexpected rewritten URL: %q", kEntry.URL)
	}
}

func TestSinglePortRepoSupport(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "Pkgfile"), []byte(`# Description: Standalone kernel repo
name=kernel
version=6.18.64
release=2
source=()
build() { true; }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := LoadRegistry(repoDir, nil)
	if err != nil {
		t.Fatalf("LoadRegistry single-port: %v", err)
	}
	if !reg.IsSinglePort || reg.SinglePort == nil || reg.SinglePort.Port.Name != "kernel" {
		t.Fatalf("unexpected single-port registry: %+v", reg)
	}
	entries, err := reg.ExpandTargets([]string{"."}, "kryx-linux/kernel")
	if err != nil || len(entries) != 1 || entries[0].Port.Name != "kernel" {
		t.Fatalf("ExpandTargets(.) = %+v, %v", entries, err)
	}
	if _, err := reg.ExpandTargets([]string{"wrong-port"}, "kryx-linux/kernel"); err == nil {
		t.Fatal("expected error for mismatched target in single-port repo")
	}
}

func TestMakeDependsSatisfiedFromBinaryIndex(t *testing.T) {
	tmp := t.TempDir()

	// Step 1: Build a prebuilt binary archive for `codegen` (simulating a remote binary repo).
	toolPorts := filepath.Join(tmp, "tool-ports")
	toolDir := filepath.Join(toolPorts, "opt", "codegen")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "Pkgfile"), []byte(`# Description: Build-time code generator
name=codegen
version=1.0
release=1
source=()
build() {
	install -d "$PKG/usr/share/codegen"
	echo generated-ok > "$PKG/usr/share/codegen/marker"
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(tmp, "remote-bins")
	var out, errOut bytes.Buffer
	toolRes, err := Build(BuildOptions{
		PortsDir:        toolPorts,
		Targets:         []string{"codegen"},
		WorkRoot:        filepath.Join(tmp, "tool-work"),
		OutDir:          binDir,
		Force:           true,
		IgnoreFootprint: true,
		IgnoreMD5Sum:    true,
		IgnoreSignature: true,
		NoStrip:         true,
		Compression:     "gz",
		SkipRootCheck:   true,
		SkipLdconfig:    true,
		ColorMode:       "never",
		Out:             &out,
		ErrOut:          &errOut,
	})
	if err != nil || len(toolRes.BuiltArchives) != 1 {
		t.Fatalf("build codegen fixture failed: err=%v out=%s errOut=%s", err, out.String(), errOut.String())
	}

	// Serve the prebuilt binary over HTTP.
	srv := httptest.NewServer(http.FileServer(http.Dir(binDir)))
	defer srv.Close()

	// Step 2: Create a single-port repo for `myapp` that has `# Make depends on: codegen`,
	// with NO `codegen` port in its ports tree (binary-only make dep!).
	appPorts := filepath.Join(tmp, "app-repo")
	if err := os.MkdirAll(appPorts, 0o755); err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Join(tmp, "rootfs")
	if err := os.MkdirAll(filepath.Join(rootDir, "var", "lib", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "var", "lib", "pkg", "db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	appPkgfile := `# Description: App built with binary make-dep
# Depends on:
# Make depends on: codegen
name=myapp
version=2.0
release=1
source=()
build() {
	test -f "` + filepath.Join(rootDir, "usr/share/codegen/marker") + `"
	install -d "$PKG/usr/share/myapp"
	cp "` + filepath.Join(rootDir, "usr/share/codegen/marker") + `" "$PKG/usr/share/myapp/built"
}
`
	if err := os.WriteFile(filepath.Join(appPorts, "Pkgfile"), []byte(appPkgfile), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed workRoot's cached index.json with the remote `codegen` binary.
	workRoot := filepath.Join(tmp, "app-work")
	indexPath := filepath.Join(workRoot, "cache", "index.json")
	if _, err := Stage(StageOptions{
		PackagesDir: binDir,
		PortsDir:    toolPorts,
		IndexPath:   indexPath,
		BaseURL:     srv.URL,
		Arch:        "x86_64",
	}); err != nil {
		t.Fatalf("Stage fixture index: %v", err)
	}

	// Step 3: Build `myapp` with --deps (binary-first). `codegen` must be installed
	// from the binary index before `myapp` builds, and `myapp` must succeed!
	appOutDir := filepath.Join(tmp, "app-dist")
	out.Reset()
	errOut.Reset()
	appRes, err := Build(BuildOptions{
		PortsDir:        appPorts,
		Targets:         []string{"myapp"},
		WorkRoot:        workRoot,
		OutDir:          appOutDir,
		Root:            rootDir,
		WithDeps:        true,
		Force:           true,
		IgnoreFootprint: true,
		IgnoreMD5Sum:    true,
		IgnoreSignature: true,
		NoStrip:         true,
		Compression:     "gz",
		SkipRootCheck:   true,
		SkipLdconfig:    true,
		ColorMode:       "never",
		Out:             &out,
		ErrOut:          &errOut,
	})
	if err != nil {
		t.Fatalf("Build myapp with binary make-dep failed: %v\nstdout=%s\nstderr=%s", err, out.String(), errOut.String())
	}
	if len(appRes.BuiltArchives) != 1 || len(appRes.MissingDeps) != 0 {
		t.Fatalf("unexpected build result: %+v", appRes)
	}

	// Step 4: Stage `myapp` and verify index.json keeps `depends` empty (runtime only)
	// while recording `makedepends: ["codegen"]`.
	finalIdxPath := filepath.Join(tmp, "final-index.json")
	if _, err := Stage(StageOptions{
		PackagesDir: appOutDir,
		PortsDir:    appPorts,
		IndexPath:   finalIdxPath,
		Arch:        "x86_64",
	}); err != nil {
		t.Fatalf("Stage myapp: %v", err)
	}
	idx, err := repo.Load(finalIdxPath)
	if err != nil {
		t.Fatalf("repo.Load: %v", err)
	}
	entry, ok := repo.Find(idx, "myapp", "x86_64")
	if !ok {
		t.Fatalf("myapp not found in final index")
	}
	if len(entry.Depends) != 0 {
		t.Fatalf("expected runtime Depends to be empty (make-deps must not leak into runtime depends), got %v", entry.Depends)
	}
	if len(entry.MakeDepends) != 1 || entry.MakeDepends[0] != "codegen" {
		t.Fatalf("expected MakeDepends=[codegen], got %v", entry.MakeDepends)
	}
}

func TestMatrixPlanWaveOrderingAndCap(t *testing.T) {
	portsDir := t.TempDir()
	writePort := func(rel, body string) {
		dir := filepath.Join(portsDir, filepath.FromSlash(rel))
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(body), 0o644)
	}
	writePort("core/base", "name=base\nversion=1.0\nrelease=1\nbuild() { true; }\n")
	writePort("opt/mid", "# Depends on: base\nname=mid\nversion=1.0\nrelease=1\nbuild() { true; }\n")
	writePort("rolling/top", "# Make depends on: mid\nname=top\nversion=1.0\nrelease=1\nbuild() { true; }\n")

	reg, err := LoadRegistry(portsDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildMatrixPlan(reg, []string{"all"}, "", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalPorts != 3 || !plan.Truncated || len(plan.Include) != 2 {
		t.Fatalf("unexpected capped matrix plan: %+v", plan)
	}
	if plan.Include[0].Ports[0] != "core/base" || plan.Include[0].Wave != 0 ||
		plan.Include[1].Ports[0] != "opt/mid" || plan.Include[1].Wave != 1 {
		t.Fatalf("unexpected topological waves: %+v", plan.Include)
	}
}
