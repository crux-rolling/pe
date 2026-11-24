package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/signing"
)

func TestRunIndexWithSignifySignature(t *testing.T) {
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "pkgs")
	stage := filepath.Join(tmp, "stage")
	if err := os.MkdirAll(filepath.Join(stage, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "usr", "bin", "demo"), []byte("ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgArchive := filepath.Join(pkgDir, "demo#1.0-1.pkg.tar.gz")
	if err := archive.Create(stage, pkgArchive, "gz"); err != nil {
		t.Fatal(err)
	}

	pubData, secData, _, err := signing.GenerateSignifyKeyPair("kpm-index-test")
	if err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(tmp, "release.pub")
	secPath := filepath.Join(tmp, "release.sec")
	_ = os.WriteFile(pubPath, pubData, 0o644)
	_ = os.WriteFile(secPath, secData, 0o600)

	indexPath := filepath.Join(tmp, "index.json")
	var stderr bytes.Buffer
	if err := runIndexWithSign(indexPath, pkgDir, "", "https://example.org/releases/download/rolling", "x86_64", false, secPath, &stderr); err != nil {
		t.Fatalf("runIndexWithSign failed: %v (stderr=%s)", err, stderr.String())
	}

	sigPath := indexPath + ".sig"
	sigBytes, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("read %s: %v", sigPath, err)
	}
	if !strings.HasPrefix(string(sigBytes), "untrusted comment:") {
		t.Fatalf("expected OpenBSD signify header in %s, got: %q", sigPath, string(sigBytes))
	}
	payload, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := signing.LoadPublicKeyFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signing.VerifyBytes([]signing.PublicKey{pk}, payload, sigBytes); err != nil {
		t.Fatalf("signify verification failed: %v", err)
	}
}
