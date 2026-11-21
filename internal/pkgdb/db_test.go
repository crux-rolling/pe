package pkgdb

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDBRoundTripAndBackup(t *testing.T) {
	root := t.TempDir()
	if err := EnsureInitialized(root); err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireLock(root, true)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer lock.Release()

	// Second exclusive lock must fail non-blocking.
	if _, err := AcquireLock(root, true); err == nil {
		t.Fatalf("expected lock contention error")
	}

	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	db.AddPkg("zlib", PkgInfo{
		Version: "1.3.1-1",
		Files: map[string]struct{}{
			"usr/":               {},
			"usr/lib/":           {},
			"usr/lib/libz.so.1":  {},
			"usr/include/zlib.h": {},
		},
	})
	if err := db.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify backup exists and DB file is readable with exact CRUX format.
	if _, err := os.Stat(filepath.Join(root, PkgDB+".backup")); err != nil {
		t.Fatalf("expected .backup file: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, PkgDB))
	if err != nil {
		t.Fatal(err)
	}
	want := "zlib\n1.3.1-1\nusr/\nusr/include/zlib.h\nusr/lib/\nusr/lib/libz.so.1\n\n"
	if string(raw) != want {
		t.Fatalf("db content mismatch:\ngot:\n%q\nwant:\n%q", string(raw), want)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.FindPkg("zlib") || reopened.Packages["zlib"].Version != "1.3.1-1" {
		t.Fatalf("reopened db missing zlib: %+v", reopened.Packages)
	}
}

func TestConflictDetectionAndSharedRemoval(t *testing.T) {
	root := t.TempDir()
	if err := EnsureInitialized(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/shared"), []byte("s"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/only-a"), []byte("a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/bin/untracked"), []byte("u"), 0o755); err != nil {
		t.Fatal(err)
	}

	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	db.AddPkg("pkga", PkgInfo{
		Version: "1.0-1",
		Files: map[string]struct{}{
			"usr/":           {},
			"usr/bin/":       {},
			"usr/bin/shared": {},
			"usr/bin/only-a": {},
		},
	})
	db.AddPkg("pkgb", PkgInfo{
		Version: "1.0-1",
		Files: map[string]struct{}{
			"usr/":           {},
			"usr/bin/":       {},
			"usr/bin/shared": {},
		},
	})

	// New package pkgc conflicting with DB (usr/bin/shared) and filesystem (usr/bin/untracked), but NOT usr/bin/ directory.
	conflicts := db.FindConflicts("pkgc", PkgInfo{
		Version: "1.0-1",
		Files: map[string]struct{}{
			"usr/":              {},
			"usr/bin/":          {},
			"usr/bin/shared":    {},
			"usr/bin/untracked": {},
			"usr/bin/fresh":     {},
		},
	})
	want := []string{"usr/bin/shared", "usr/bin/untracked"}
	if !reflect.DeepEqual(conflicts, want) {
		t.Fatalf("FindConflicts = %v, want %v", conflicts, want)
	}

	// Upgrading pkga should not flag usr/bin/only-a as a conflict, while usr/bin/shared owned by pkgb IS excluded because pkga also owns it.
	upgradeConflicts := db.FindConflicts("pkga", PkgInfo{
		Version: "2.0-1",
		Files: map[string]struct{}{
			"usr/":           {},
			"usr/bin/":       {},
			"usr/bin/only-a": {},
		},
	})
	if len(upgradeConflicts) != 0 {
		t.Fatalf("unexpected upgrade conflicts: %v", upgradeConflicts)
	}

	// Removing pkga must delete usr/bin/only-a but keep usr/bin/shared (still owned by pkgb).
	var errBuf bytes.Buffer
	db.RemovePkg("pkga", nil, &errBuf)
	if _, err := os.Lstat(filepath.Join(root, "usr/bin/only-a")); !os.IsNotExist(err) {
		t.Fatalf("expected usr/bin/only-a to be removed")
	}
	if _, err := os.Lstat(filepath.Join(root, "usr/bin/shared")); err != nil {
		t.Fatalf("expected usr/bin/shared to remain because pkgb still references it: %v", err)
	}
}

func TestPkgaddConfRules(t *testing.T) {
	// Self-contained copy of standard CRUX pkgadd.conf (including \w) so tests
	// pass in standalone CI checkouts without sibling reference directories.
	cruxPkgaddConf := `#
# /etc/pkgadd.conf: pkgadd(8) configuration
#

UPGRADE		^etc/.*$		NO
UPGRADE		^var/log/.*$		NO
UPGRADE		^var/spool/\w*cron/.*$	NO
UPGRADE		^var/run/utmp$		NO

UPGRADE		^etc/ports/drivers/.*$	YES
UPGRADE		^etc/X11/.*$		YES

UPGRADE		^etc/rc.*$		YES
UPGRADE		^etc/rc\.local$		NO
UPGRADE		^etc/rc\.modules$	NO
UPGRADE		^etc/rc\.conf$		NO
UPGRADE		^etc/rc\.d/net$		NO
UPGRADE		^etc/rc\.d/wlan$	NO

UPGRADE		^etc/udev/rules.d/.*$	YES
UPGRADE		^etc/udev/rules.d/1.*$	NO
UPGRADE		^etc/udev/hwdb.d/.*$	YES
UPGRADE		^etc/udev/hwdb.bin$	YES

UPGRADE		^etc/ssl/cert.pem$	YES

UPGRADE		^var/lib/pkg/prt-get.locker$	NO
`
	refConf := filepath.Join(t.TempDir(), "pkgadd.conf")
	if err := os.WriteFile(refConf, []byte(cruxPkgaddConf), 0o644); err != nil {
		t.Fatal(err)
	}
	rules, err := ReadRules("/", refConf)
	if err != nil {
		t.Fatalf("ReadRules on CRUX pkgadd.conf fixture failed: %v", err)
	}
	if len(rules) == 0 {
		t.Fatalf("expected non-empty rules from CRUX pkgadd.conf fixture")
	}

	files := map[string]struct{}{
		"etc/passwd":             {}, // UPGRADE NO (^etc/.*$)
		"etc/X11/xorg.conf":      {}, // UPGRADE YES (^etc/X11/.*$ overrides ^etc/.*$)
		"etc/rc.conf":            {}, // UPGRADE NO (^etc/rc\.conf$ overrides ^etc/rc.*$)
		"etc/rc.single":          {}, // UPGRADE YES (^etc/rc.*$)
		"var/spool/cron/crontabs": {}, // UPGRADE NO (^var/spool/\w*cron/.*$)
		"usr/bin/foo":            {}, // default YES
	}
	keep := MakeKeepList(files, rules)

	for _, mustKeep := range []string{"etc/passwd", "etc/rc.conf", "var/spool/cron/crontabs"} {
		if _, ok := keep[mustKeep]; !ok {
			t.Errorf("expected %s to be in keep list", mustKeep)
		}
	}
	for _, mustOverwrite := range []string{"etc/X11/xorg.conf", "etc/rc.single", "usr/bin/foo"} {
		if _, ok := keep[mustOverwrite]; ok {
			t.Errorf("expected %s NOT to be in keep list", mustOverwrite)
		}
	}

	// Test INSTALL rules too.
	tmpConf := filepath.Join(t.TempDir(), "pkgadd.conf")
	if err := os.WriteFile(tmpConf, []byte("INSTALL ^usr/share/locale/.*$ NO\nINSTALL ^usr/share/locale/en/.*$ YES\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	installRules, err := ReadRules("/", tmpConf)
	if err != nil {
		t.Fatal(err)
	}
	info := PkgInfo{
		Version: "1.0-1",
		Files: map[string]struct{}{
			"usr/bin/app":                {},
			"usr/share/locale/fr/app.mo": {},
			"usr/share/locale/en/app.mo": {},
		},
	}
	nonInstall := ApplyInstallRules(&info, installRules)
	if _, ok := nonInstall["usr/share/locale/fr/app.mo"]; !ok {
		t.Fatalf("expected fr locale to be excluded")
	}
	if _, ok := info.Files["usr/share/locale/en/app.mo"]; !ok {
		t.Fatalf("expected en locale to remain in install set")
	}
	if _, ok := info.Files["usr/share/locale/fr/app.mo"]; ok {
		t.Fatalf("expected fr locale to be removed from install set")
	}
}
