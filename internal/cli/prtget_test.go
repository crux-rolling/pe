package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMulticallPrtGetShim(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "var", "lib", "pkg")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbContent := "libinput\n1.27.0-1\nusr/lib/libinput.so\n\nxorg-xwayland\n24.1.4-1\nusr/bin/Xwayland\n\n"
	if err := os.WriteFile(filepath.Join(dbDir, "db"), []byte(dbContent), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := RunWithArgv0Options("/usr/bin/prt-get", []string{"--install-root=" + root, "isinst", "libinput", "xorg-xwayland"}, &out, &errBuf, RuntimeOptions{SkipRootCheck: true})
	if code != 0 || !strings.Contains(out.String(), "package libinput is installed") {
		t.Fatalf("expected exit 0 for installed pkgs, got code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	out.Reset()
	errBuf.Reset()
	code = RunWithArgv0Options("/usr/bin/prt-get", []string{"--install-root=" + root, "isinst", "libinput", "missing-pkg"}, &out, &errBuf, RuntimeOptions{SkipRootCheck: true})
	if code == 0 || !strings.Contains(out.String(), "package missing-pkg is not installed") {
		t.Fatalf("expected non-zero exit when any pkg missing, got code=%d out=%q", code, out.String())
	}

	out.Reset()
	code = RunWithArgv0Options("/usr/bin/prt-get", []string{"--install-root=" + root, "current", "libinput"}, &out, &errBuf, RuntimeOptions{SkipRootCheck: true})
	if code != 0 || strings.TrimSpace(out.String()) != "1.27.0-1" {
		t.Fatalf("expected version 1.27.0-1, got code=%d out=%q", code, out.String())
	}

	out.Reset()
	code = RunWithArgv0Options("/usr/bin/prt-get", []string{"--install-root=" + root, "listinst", "-v"}, &out, &errBuf, RuntimeOptions{SkipRootCheck: true})
	if code != 0 || !strings.Contains(out.String(), "xorg-xwayland 24.1.4-1") {
		t.Fatalf("unexpected listinst output: code=%d out=%q", code, out.String())
	}

	out.Reset()
	code = RunWithArgv0Options("/usr/bin/prt-get", []string{"--install-root=" + root, "printf", `%v\n`, "--filter=libinput"}, &out, &errBuf, RuntimeOptions{SkipRootCheck: true})
	if code != 0 || strings.TrimSpace(out.String()) != "1.27.0" {
		t.Fatalf("unexpected printf output: code=%d out=%q", code, out.String())
	}
}
