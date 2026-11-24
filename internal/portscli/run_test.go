package portscli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortsCLIEndToEndWithKPMIntegration(t *testing.T) {
	pkgBody := "# Description: Demo package for ports\nname=demo\nversion=2.0\nrelease=1\nbuild() { true; }\n"
	sum := md5.Sum([]byte(pkgBody))
	pkgMD5 := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/REPO":
			fmt.Fprintf(w, "d:demo\nf:0644:%s:demo/Pkgfile\n", pkgMD5)
		case "/demo/Pkgfile":
			_, _ = w.Write([]byte(pkgBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tmp := t.TempDir()
	portsDir := filepath.Join(tmp, "ports")
	rootDir := filepath.Join(tmp, "rootfs")
	cfgPath := filepath.Join(tmp, "sources.list")
	kpmIniPath := filepath.Join(tmp, "kpm.ini")
	resolvPath := filepath.Join(tmp, "resolv.conf")

	if err := os.MkdirAll(filepath.Join(rootDir, "var", "lib", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootDir, "var", "lib", "pkg", "db"), []byte("demo\n1.0-1\nusr/bin/demo\n\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kpmIniPath, []byte(fmt.Sprintf("[paths]\nroot = %s\nports_dir = %s\ncollections = demo\n", rootDir, portsDir)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resolvPath, []byte("nameserver 1.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("httpup demo %s/\n", srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PORTS_RESOLV_CONF", resolvPath)
	t.Setenv("KPM_CONFIG", kpmIniPath)

	// 1. ports check should pass and NOT create portsDir
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"-c", cfgPath, "check"}, &out, &errOut, "0.6.0")
	if code != 0 {
		t.Fatalf("ports check exited %d\nstdout=%s\nstderr=%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(portsDir); !os.IsNotExist(err) {
		t.Fatal("ports check should not touch ports directory")
	}

	// 2. ports status should show 'not synced'
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"status", "-c", cfgPath}, &out, &errOut, "0.6.0")
	if code != 0 || !strings.Contains(out.String(), "not synced") {
		t.Fatalf("ports status exited %d: out=%s err=%s", code, out.String(), errOut.String())
	}

	// 3. ports -u -j 2 demo should sync cleanly into kpm.ini's ports_dir
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"-c", cfgPath, "-u", "-j", "2", "demo"}, &out, &errOut, "0.6.0")
	if code != 0 || !strings.Contains(out.String(), "[DONE]") {
		t.Fatalf("ports -u exited %d\nstdout=%s\nstderr=%s", code, out.String(), errOut.String())
	}

	// 4. ports -l should list demo/demo
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"-l"}, &out, &errOut, "0.6.0")
	if code != 0 || strings.TrimSpace(out.String()) != "demo/demo" {
		t.Fatalf("ports -l exited %d: out=%q err=%s", code, out.String(), errOut.String())
	}

	// 5. ports -s should find demo by description
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"-s", "Demo package"}, &out, &errOut, "0.6.0")
	if code != 0 || !strings.Contains(out.String(), "demo/demo 2.0-1 - Demo package for ports") {
		t.Fatalf("ports -s exited %d: out=%q err=%s", code, out.String(), errOut.String())
	}

	// 6. ports -d should compare local port (2.0-1) against installed kpm DB (1.0-1)
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"-d"}, &out, &errOut, "0.6.0")
	if code != 0 || !strings.Contains(out.String(), "2.0-1") || !strings.Contains(out.String(), "1.0-1") {
		t.Fatalf("ports -d exited %d: out=%q err=%s", code, out.String(), errOut.String())
	}

	// 7. ports update (second run) should report [SKIP] (up-to-date)
	out.Reset()
	errOut.Reset()
	code = Run(context.Background(), []string{"update", "-c", cfgPath}, &out, &errOut, "0.6.0")
	if code != 0 || !strings.Contains(out.String(), "[SKIP]") {
		t.Fatalf("ports update exited %d: out=%s err=%s", code, out.String(), errOut.String())
	}
}
