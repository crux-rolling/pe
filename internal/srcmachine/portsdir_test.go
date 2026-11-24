package srcmachine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePortsDirPriority(t *testing.T) {
	tmp := t.TempDir()
	cliDir := filepath.Join(tmp, "cli-ports")
	kryxEnvDir := filepath.Join(tmp, "kryx-env-ports")
	kpmEnvDir := filepath.Join(tmp, "kpm-env-ports")
	cfgDir := filepath.Join(tmp, "cfg-ports")
	wsDir := filepath.Join(tmp, "workspace")
	wsPorts := filepath.Join(wsDir, "ports")
	wsColDir := filepath.Join(tmp, "ws-collections")
	wsSingleDir := filepath.Join(tmp, "ws-single")
	emptyDir := filepath.Join(tmp, "empty-ws")

	for _, d := range []string{
		cliDir, kryxEnvDir, kpmEnvDir, cfgDir,
		wsPorts, filepath.Join(wsColDir, "opt"),
		wsSingleDir, emptyDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wsSingleDir, "Pkgfile"), []byte("name=single\nversion=1\nrelease=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	kpmIni := filepath.Join(tmp, "kpm.ini")
	if err := os.WriteFile(kpmIni, []byte("[paths]\nports_dir = "+cfgDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Priority 1: CLI --ports-dir beats everything.
	t.Setenv("KRYX_PORTS_DIR", kryxEnvDir)
	t.Setenv("KPM_PORTS_DIR", kpmEnvDir)
	t.Setenv("KPM_CONFIG", kpmIni)
	got, err := ResolvePortsDir(cliDir, kpmIni, wsDir)
	if err != nil || got != cliDir {
		t.Fatalf("priority 1 failed: got=%q err=%v want=%q", got, err, cliDir)
	}

	// Priority 2a: KRYX_PORTS_DIR beats KPM_PORTS_DIR, kpm.ini, workspace.
	got, err = ResolvePortsDir("", kpmIni, wsDir)
	if err != nil || got != kryxEnvDir {
		t.Fatalf("priority 2a failed: got=%q err=%v want=%q", got, err, kryxEnvDir)
	}

	// Priority 2b: KPM_PORTS_DIR beats kpm.ini, workspace.
	t.Setenv("KRYX_PORTS_DIR", "")
	got, err = ResolvePortsDir("", kpmIni, wsDir)
	if err != nil || got != kpmEnvDir {
		t.Fatalf("priority 2b failed: got=%q err=%v want=%q", got, err, kpmEnvDir)
	}

	// Priority 3: kpm.ini [paths] ports_dir beats workspace.
	t.Setenv("KPM_PORTS_DIR", "")
	got, err = ResolvePortsDir("", kpmIni, wsDir)
	if err != nil || got != cfgDir {
		t.Fatalf("priority 3 failed: got=%q err=%v want=%q", got, err, cfgDir)
	}

	// Priority 4a: Workspace ./ports auto-detection when config points to nonexistent dir.
	badIni := filepath.Join(tmp, "missing-ports.ini")
	_ = os.WriteFile(badIni, []byte("[paths]\nports_dir = "+filepath.Join(tmp, "does-not-exist")+"\n"), 0o644)
	t.Setenv("KPM_CONFIG", badIni)
	got, err = ResolvePortsDir("", "", wsDir)
	if err != nil || got != wsPorts {
		t.Fatalf("priority 4a (./ports) failed: got=%q err=%v want=%q", got, err, wsPorts)
	}

	// Priority 4b: Workspace ./opt collection directory auto-detection.
	got, err = ResolvePortsDir("", "", wsColDir)
	if err != nil || got != wsColDir {
		t.Fatalf("priority 4b (./opt) failed: got=%q err=%v want=%q", got, err, wsColDir)
	}

	// Priority 4c: Workspace single-port ./Pkgfile auto-detection.
	got, err = ResolvePortsDir("", "", wsSingleDir)
	if err != nil || got != wsSingleDir {
		t.Fatalf("priority 4c (./Pkgfile) failed: got=%q err=%v want=%q", got, err, wsSingleDir)
	}

	// Non-existent explicit CLI --ports-dir MUST error immediately.
	if _, err := ResolvePortsDir(filepath.Join(tmp, "nope"), "", wsDir); err == nil || !strings.Contains(err.Error(), "--ports-dir") {
		t.Fatalf("expected error on missing --ports-dir, got %v", err)
	}

	// When no /usr/ports exists on host and cwd is empty, must return actionable error.
	if _, statErr := os.Stat("/usr/ports"); os.IsNotExist(statErr) {
		if _, err := ResolvePortsDir("", "", emptyDir); err == nil || !strings.Contains(err.Error(), "KRYX_PORTS_DIR") {
			t.Fatalf("expected clear error mentioning KRYX_PORTS_DIR, got %v", err)
		}
	}
}
