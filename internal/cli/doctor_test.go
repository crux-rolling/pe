package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorDetectsShadowedPathBinaryAndValidDB(t *testing.T) {
	tmp := t.TempDir()
	rootDir := filepath.Join(tmp, "root")
	if err := os.MkdirAll(filepath.Join(rootDir, "var", "lib", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "var", "lib", "pkg", "db"), nil, 0o444); err != nil {
		t.Fatal(err)
	}

	// Simulate legacy /sbin/pe shadowing /usr/bin/pe in PATH.
	fakeSbin := filepath.Join(tmp, "sbin")
	fakeUsrBin := filepath.Join(tmp, "usr", "bin")
	if err := os.MkdirAll(fakeSbin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fakeUsrBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeSbin, "pe"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeUsrBin, "pe"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", fakeSbin+string(os.PathListSeparator)+fakeUsrBin)

	cfgPath := filepath.Join(tmp, "pe.ini")
	if err := os.WriteFile(cfgPath, []byte("[paths]\nroot = "+rootDir+"\nports_dir = "+filepath.Join(tmp, "ports")+"\ncache_dir = "+filepath.Join(tmp, "cache")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := RunWithOptions([]string{"--config", cfgPath, "--no-color", "doctor"}, &out, &errOut, RuntimeOptions{SkipRootCheck: true, SkipLdconfig: true})
	if code != 0 {
		t.Fatalf("doctor exited %d: out=%s err=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "multiple copies in PATH") || !strings.Contains(out.String(), "hash -r") {
		t.Fatalf("expected shadowed PATH + hash -r warning in doctor output:\n%s", out.String())
	}
}
