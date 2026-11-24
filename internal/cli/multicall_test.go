package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryx-linux/kpm/internal/archive"
)

func buildTestArchive(t *testing.T, outDir, name, ver, rel string, files map[string]string) string {
	t.Helper()
	stage := t.TempDir()
	for relPath, content := range files {
		full := filepath.Join(stage, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkgPath := filepath.Join(outDir, archive.FormatPkgFilename(name, ver, rel, "gz"))
	if err := archive.Create(stage, pkgPath, "gz"); err != nil {
		t.Fatalf("archive.Create(%s): %v", pkgPath, err)
	}
	return pkgPath
}

func TestMulticallPkgaddPkginfoPkgrmRejmerge(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	pkgs := filepath.Join(tmp, "pkgs")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkgs, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write CRUX /etc/pkgadd.conf preserving /etc/demo.conf on upgrade.
	pkgaddConf := filepath.Join(root, "etc", "pkgadd.conf")
	if err := os.WriteFile(pkgaddConf, []byte("UPGRADE\t^etc/demo\\.conf$\tNO\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	v1 := buildTestArchive(t, pkgs, "demo", "1.0", "1", map[string]string{
		"etc/demo.conf": "mode=v1\n",
		"usr/bin/demo":  "#!/bin/sh\necho v1\n",
	})
	v2 := buildTestArchive(t, pkgs, "demo", "2.0", "1", map[string]string{
		"etc/demo.conf": "mode=v2\n",
		"usr/bin/demo":  "#!/bin/sh\necho v2\n",
	})

	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}
	var out, errOut bytes.Buffer

	// 1. pkgadd install
	code := RunWithArgv0Options("/usr/bin/pkgadd", []string{"-r", root, "-c", pkgaddConf, v1}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgadd install failed (%d): %s %s", code, out.String(), errOut.String())
	}

	// 2. pkginfo -i
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-r", root, "-i"}, &out, &errOut, rt)
	if code != 0 || strings.TrimSpace(out.String()) != "demo 1.0-1" {
		t.Fatalf("pkginfo -i = (%d) %q err=%q", code, out.String(), errOut.String())
	}

	// 3. pkginfo -l demo & -l <archive>
	out.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-r", root, "-l", "demo"}, &out, &errOut, rt)
	if code != 0 || !strings.Contains(out.String(), "etc/demo.conf") || !strings.Contains(out.String(), "usr/bin/demo") {
		t.Fatalf("pkginfo -l demo = (%d) %q", code, out.String())
	}

	// 4. pkginfo -o (exact stock CRUX header + no owner message)
	out.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-r", root, "-o", "usr/bin/demo$"}, &out, &errOut, rt)
	if code != 0 || !strings.Contains(out.String(), "Package") || !strings.Contains(out.String(), "demo") {
		t.Fatalf("pkginfo -o = (%d) %q", code, out.String())
	}
	out.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-r", root, "-o", "nonexistent_file_xyz"}, &out, &errOut, rt)
	if code != 0 || strings.TrimSpace(out.String()) != "pkginfo: no owner(s) found" {
		t.Fatalf("pkginfo -o miss = (%d) %q", code, out.String())
	}

	// 5. pkginfo -f on archive and on directory
	out.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-f", v1}, &out, &errOut, rt)
	if code != 0 || !strings.Contains(out.String(), "etc/demo.conf") {
		t.Fatalf("pkginfo -f archive = (%d) %q err=%q", code, out.String(), errOut.String())
	}
	out.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-f", filepath.Join(root, "usr")}, &out, &errOut, rt)
	if code != 0 || !strings.Contains(out.String(), "bin/demo") {
		t.Fatalf("pkginfo -f dir = (%d) %q err=%q", code, out.String(), errOut.String())
	}

	// 6. pkgadd -u triggers rejected file under /var/lib/pkg/rejected/etc/demo.conf
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgadd", []string{"-r", root, "-c", pkgaddConf, "-u", v2}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgadd -u failed (%d): out=%q err=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "pkgadd: rejecting etc/demo.conf") {
		t.Fatalf("expected pkgadd rejecting log, got: %q", out.String())
	}
	rejFile := filepath.Join(root, "var", "lib", "pkg", "rejected", "etc", "demo.conf")
	if _, err := os.Stat(rejFile); err != nil {
		t.Fatalf("expected rejected file at %s: %v", rejFile, err)
	}

	// 7. kpm rejected --upgrade promotes rejected config
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("kpm", []string{"--root", root, "rejected", "--upgrade", "all"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("kpm rejected --upgrade failed (%d): %s %s", code, out.String(), errOut.String())
	}
	gotConf, _ := os.ReadFile(filepath.Join(root, "etc", "demo.conf"))
	if strings.TrimSpace(string(gotConf)) != "mode=v2" {
		t.Fatalf("expected upgraded demo.conf mode=v2, got %q", string(gotConf))
	}

	// 8. pkgrm removes package cleanly
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgrm", []string{"-r", root, "demo"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgrm failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	code = RunWithArgv0Options("pkginfo", []string{"-r", root, "-i"}, &out, &errOut, rt)
	if code != 0 || strings.TrimSpace(out.String()) != "" {
		t.Fatalf("expected empty installed list after pkgrm, got %q", out.String())
	}
}

func TestMulticallPkgmkParity(t *testing.T) {
	tmp := t.TempDir()
	portDir := filepath.Join(tmp, "hello")
	pkgOutDir := filepath.Join(tmp, "out")
	srcCache := filepath.Join(tmp, "src")
	workDir := filepath.Join(tmp, "work")
	if err := os.MkdirAll(portDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portDir, "data.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create .32bit marker to verify PKGMK_ARCH=32 propagation.
	if err := os.WriteFile(filepath.Join(portDir, ".32bit"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pkgfile := `# Description: Hello test port
name=hello
version=1.0
release=1
source=(data.txt)
build() {
    mkdir -p "$PKG/usr/share/hello"
    cp "$SRC/data.txt" "$PKG/usr/share/hello/data.txt"
    echo "$PKGMK_ARCH" > "$PKG/usr/share/hello/arch.txt"
}
`
	if err := os.WriteFile(filepath.Join(portDir, "Pkgfile"), []byte(pkgfile), 0o644); err != nil {
		t.Fatal(err)
	}

	pkgmkConf := filepath.Join(tmp, "pkgmk.conf")
	confText := "PKGMK_SOURCE_DIR=" + srcCache + "\nPKGMK_PACKAGE_DIR=" + pkgOutDir + "\nPKGMK_WORK_DIR=" + workDir + "\nPKGMK_COMPRESSION_MODE=gz\nPKGMK_IGNORE_SIGNATURE=yes\n"
	if err := os.WriteFile(pkgmkConf, []byte(confText), 0o644); err != nil {
		t.Fatal(err)
	}

	origWD, _ := os.Getwd()
	if err := os.Chdir(portDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWD) }()

	rt := RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true}
	var out, errOut bytes.Buffer

	// 1. pkgmk -utd before build should exit 1 ("not up to date")
	code := RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-utd"}, &out, &errOut, rt)
	if code != 1 || !strings.Contains(out.String(), "not up to date") {
		t.Fatalf("pkgmk -utd before build = (%d) out=%q err=%q", code, out.String(), errOut.String())
	}

	// 2. pkgmk -um creates .md5sum, pkgmk -cm verifies it
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-um"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgmk -um failed (%d): %s", code, errOut.String())
	}
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-cm"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgmk -cm failed (%d): %s", code, errOut.String())
	}

	// 3. Corrupt .md5sum -> pkgmk -cm MUST return upstream exit code 6 (E_MD5)
	if err := os.WriteFile(filepath.Join(portDir, ".md5sum"), []byte("00000000000000000000000000000000  data.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-cm"}, &out, &errOut, rt)
	if code != 6 {
		t.Fatalf("expected exit code 6 (E_MD5) on md5 mismatch, got %d", code)
	}
	// Restore valid .md5sum
	_ = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-um"}, &out, &errOut, rt)

	// 4. pkgmk -eo extracts sources into $SRC
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-eo"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgmk -eo failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(workDir, "hello", "src", "data.txt")); err != nil {
		t.Fatalf("expected extracted data.txt in work dir: %v", err)
	}

	// 5. Build package in CWD with 0 positional args (creates .footprint automatically)
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-ns"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgmk build failed (%d): out=%q err=%q", code, out.String(), errOut.String())
	}
	builtPkg := filepath.Join(pkgOutDir, "hello#1.0-1.pkg.tar.gz")
	if _, err := os.Stat(builtPkg); err != nil {
		t.Fatalf("expected built archive %s: %v", builtPkg, err)
	}
	// Check .footprint was created
	fpBytes, err := os.ReadFile(filepath.Join(portDir, ".footprint"))
	if err != nil || !strings.Contains(string(fpBytes), "usr/share/hello/arch.txt") {
		t.Fatalf("expected .footprint with arch.txt, err=%v content=%q", err, string(fpBytes))
	}

	// 6. pkgmk -utd after build should exit 0 ("is up to date")
	out.Reset()
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-utd"}, &out, &errOut, rt)
	if code != 0 || !strings.Contains(out.String(), "is up to date") {
		t.Fatalf("pkgmk -utd after build = (%d) out=%q", code, out.String())
	}

	// 7. pkgmk -uf updates footprint cleanly
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-uf"}, &out, &errOut, rt)
	if code != 0 || !strings.Contains(out.String(), "Footprint updated") {
		t.Fatalf("pkgmk -uf = (%d) out=%q err=%q", code, out.String(), errOut.String())
	}

	// 8. pkgmk -c removes built archive
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0Options("pkgmk", []string{"-cf", pkgmkConf, "-c"}, &out, &errOut, rt)
	if code != 0 {
		t.Fatalf("pkgmk -c failed (%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(builtPkg); !os.IsNotExist(err) {
		t.Fatalf("expected %s removed by pkgmk -c", builtPkg)
	}
}

func TestMulticallPortsParity(t *testing.T) {
	tmp := t.TempDir()
	portsDir := filepath.Join(tmp, "ports")
	root := filepath.Join(tmp, "root")
	confDir := filepath.Join(tmp, "etc-ports")
	driversDir := filepath.Join(confDir, "drivers")
	if err := os.MkdirAll(filepath.Join(portsDir, "core", "zlib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var", "lib", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(driversDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(portsDir, "core", "zlib", "Pkgfile"), []byte("name=zlib\nversion=1.3.1\nrelease=2\nbuild() { :; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "var", "lib", "pkg", "db"), []byte("zlib\n1.3.1-1\nusr/lib/libz.so\n\norphan\n0.1-1\nusr/bin/orphan\n\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	// Custom driver test
	driverScript := filepath.Join(driversDir, "mock")
	if err := os.WriteFile(driverScript, []byte("#!/bin/sh\necho \"MOCK_DRIVER PORTS_DIR=$PORTS_DIR CONF=$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "core.mock"), []byte("url=mock\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	kpmIni := filepath.Join(tmp, "kpm.ini")
	iniText := "[paths]\nroot = " + root + "\nports_dir = " + portsDir + "\nports_conf_dir = " + confDir + "\nports_drivers_dir = " + driversDir + "\n"
	if err := os.WriteFile(kpmIni, []byte(iniText), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KPM_CONFIG", kpmIni)

	var out, errOut bytes.Buffer
	// ports -l
	code := RunWithArgv0("ports", []string{"-l"}, &out, &errOut)
	if code != 0 || strings.TrimSpace(out.String()) != "core/zlib" {
		t.Fatalf("ports -l = (%d) %q err=%q", code, out.String(), errOut.String())
	}

	// ports -d
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0("ports", []string{"-d"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "Collection") || !strings.Contains(out.String(), "1.3.1-2") || !strings.Contains(out.String(), "MISSING") {
		t.Fatalf("ports -d = (%d) out=%q err=%q", code, out.String(), errOut.String())
	}

	// ports -u core
	out.Reset()
	errOut.Reset()
	code = RunWithArgv0("ports", []string{"-u", "core"}, &out, &errOut)
	if code != 0 || !strings.Contains(out.String(), "MOCK_DRIVER PORTS_DIR="+portsDir) {
		t.Fatalf("ports -u core = (%d) out=%q err=%q", code, out.String(), errOut.String())
	}
}
