package tree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kryx-linux/kpm/internal/portscli/kpmcompat"
)

func TestParseListSearchAndDiff(t *testing.T) {
	portsDir := t.TempDir()
	writePort := func(col, dirName, content string) {
		dir := filepath.Join(portsDir, col, dirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "Pkgfile"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePort("core", "zlib", `# Description: Compression library
# URL: https://zlib.net
# Depends on: glibc

_base=1.3
name=zlib
version=${_base}.1
release=2
source=(https://zlib.net/$name-$version.tar.gz)
build() { true; }
`)
	writePort("opt", "zlib", `# Description: Opt shadow zlib
name=zlib
version=9.9.9
release=1
build() { true; }
`)
	writePort("opt", "ripgrep", `# Description: Fast recursive grep tool
name=ripgrep
version=14.1.0
release=1
build() { true; }
`)

	entries, err := ListEntries(portsDir, []string{"core", "opt"})
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 3 || entries[0] != "core/zlib" || entries[1] != "opt/ripgrep" || entries[2] != "opt/zlib" {
		t.Fatalf("unexpected entries: %v", entries)
	}

	all, err := DiscoverAll(portsDir, []string{"core", "opt"})
	if err != nil {
		t.Fatalf("DiscoverAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 ports, got %d", len(all))
	}

	// Verify pure-Go ${var} expansion on core/zlib
	pmap := PriorityMap(all, []string{"core", "opt"})
	if pmap["zlib"].Collection != "core" || pmap["zlib"].FullVersion() != "1.3.1-2" {
		t.Fatalf("priority/expansion mismatch: %+v", pmap["zlib"])
	}

	// Search by description
	found := Search(all, "recursive grep")
	if len(found) != 1 || found[0].Name != "ripgrep" {
		t.Fatalf("Search mismatch: %+v", found)
	}

	// Diff against kpm DB
	db := &kpmcompat.DB{
		Packages: map[string]kpmcompat.InstalledPackage{
			"zlib":    {Name: "zlib", Version: "1.3.1-1"},
			"ripgrep": {Name: "ripgrep", Version: "14.1.0-1"},
		},
	}
	diffs := Diff(db, all, []string{"core", "opt"})
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %+v", diffs)
	}
	if diffs[0].Name != "zlib" || diffs[0].Collection != "core" || diffs[0].PortVer != "1.3.1-2" || diffs[0].Installed != "1.3.1-1" || diffs[0].Compare <= 0 {
		t.Fatalf("unexpected diff: %+v", diffs[0])
	}
}
