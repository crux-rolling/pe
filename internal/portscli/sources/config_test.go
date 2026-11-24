package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultTemplate(t *testing.T) {
	templatePath := filepath.Join("..", "..", "..", "configs", "sources.list")
	res := Load(Options{
		ConfigPath: templatePath,
		DropInDir:  filepath.Join(t.TempDir(), "nonexistent.d"),
	})
	if !res.OK() {
		t.Fatalf("expected default template to lint cleanly, got errors: %v", res.Errors)
	}
	if len(res.Sources) != 5 {
		t.Fatalf("expected 5 collections in default template, got %d", len(res.Sources))
	}

	want := []struct {
		proto Protocol
		name  string
		url   string
		dest  string
	}{
		{ProtoGit, "rolling", "https://github.com/kryx-linux/ports.git", "/usr/ports/rolling-repo"},
		{ProtoGit, "core", "https://github.com/kryx-linux/core.git", "/usr/ports/core"},
		{ProtoGit, "opt", "https://github.com/kryx-linux/opt.git", "/usr/ports/opt"},
		{ProtoGit, "xorg", "https://github.com/kryx-linux/xorg.git", "/usr/ports/xorg"},
		{ProtoGit, "contrib", "https://github.com/kryx-linux/contrib.git", "/usr/ports/contrib"},
	}

	for i, w := range want {
		got := res.Sources[i]
		if got.Protocol != w.proto || got.Collection != w.name || got.URL != w.url || got.DestDir != w.dest {
			t.Errorf("source[%d] = %+v, want %+v", i, got, w)
		}
	}
}

func TestLoadWithDropInsAndInlineComments(t *testing.T) {
	dir := t.TempDir()
	mainCfg := filepath.Join(dir, "sources.list")
	dropDir := filepath.Join(dir, "sources.list.d")
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mainContent := `
# Primary ports list
git core https://git.crux.nu/ports/core.git   # inline comment

httpup custom https://example.org/ports/#subport /srv/ports/custom # custom dest
`
	if err := os.WriteFile(mainCfg, []byte(mainContent), 0o644); err != nil {
		t.Fatal(err)
	}

	drop1 := filepath.Join(dropDir, "10-contrib.list")
	if err := os.WriteFile(drop1, []byte("rsync contrib rsync.crux.nu::ports/crux-3.8/contrib/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-.list files should be ignored
	ignored := filepath.Join(dropDir, "README.txt")
	if err := os.WriteFile(ignored, []byte("not a valid config line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := Load(Options{
		ConfigPath: mainCfg,
		PortsDir:   "/opt/ports",
	})
	if !res.OK() {
		t.Fatalf("unexpected lint errors: %v", res.Errors)
	}
	if len(res.Sources) != 3 {
		t.Fatalf("expected 3 sources, got %d", len(res.Sources))
	}

	// Verify #subport fragment was preserved while inline comment was stripped.
	if res.Sources[1].URL != "https://example.org/ports/#subport" {
		t.Errorf("URL fragment not preserved: got %q", res.Sources[1].URL)
	}
	if res.Sources[1].DestDir != "/srv/ports/custom" {
		t.Errorf("custom dest mismatch: got %q", res.Sources[1].DestDir)
	}
	if res.Sources[0].DestDir != "/opt/ports/core" {
		t.Errorf("custom PortsDir not applied: got %q", res.Sources[0].DestDir)
	}
	if res.Sources[2].Protocol != ProtoRsync || res.Sources[2].Collection != "contrib" {
		t.Errorf("drop-in source mismatch: %+v", res.Sources[2])
	}
}

func TestLintErrors(t *testing.T) {
	input := `
# bad protocol
svn core https://example.org/core
# markdown url
git badmd [https://git.crux.nu/ports/core.git](https://git.crux.nu/ports/core.git)
# too few fields
git onlytwo
# relative destination
git reldest https://git.crux.nu/ports/opt.git relative/path
# traversal collection name
git ../escape https://git.crux.nu/ports/xorg.git
# valid then duplicate
git dup https://git.crux.nu/ports/core.git
httpup dup https://example.org/ports/
`
	res := ParseReader(strings.NewReader(input), "test.list", "/usr/ports")
	if res.OK() {
		t.Fatal("expected lint errors, got none")
	}
	if len(res.Errors) != 6 {
		t.Fatalf("expected 6 lint errors, got %d: %v", len(res.Errors), res.Errors)
	}
	if len(res.Sources) != 1 || res.Sources[0].Collection != "dup" {
		t.Fatalf("expected only first 'dup' valid source, got %+v", res.Sources)
	}
}

func TestFilter(t *testing.T) {
	sources := []Source{
		{Collection: "core"},
		{Collection: "opt"},
		{Collection: "xorg"},
	}

	all, err := Filter(sources, nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("Filter(nil) = %v, %v", all, err)
	}

	sub, err := Filter(sources, []string{"xorg", "core", "xorg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sub) != 2 || sub[0].Collection != "xorg" || sub[1].Collection != "core" {
		t.Fatalf("unexpected filtered result: %+v", sub)
	}

	if _, err := Filter(sources, []string{"core", "missing"}); err == nil {
		t.Fatal("expected error for unknown collection")
	}
}

func TestLoadKPMConfig(t *testing.T) {
	dir := t.TempDir()
	kpmIni := filepath.Join(dir, "kpm.ini")
	content := `
# kpm config comment
[paths]
root = /mnt/crux
ports_dir = /srv/ports # inline comment
collections = core, opt, rolling

[build]
jobs = 8

[ui]
color = never
`
	if err := os.WriteFile(kpmIni, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadKPMConfig(kpmIni, true)
	if err != nil {
		t.Fatalf("LoadKPMConfig: %v", err)
	}
	if cfg.Root != "/mnt/crux" || cfg.PortsDir != "/srv/ports" || cfg.Color != "never" {
		t.Fatalf("unexpected KPMConfig: %+v", cfg)
	}
	if len(cfg.Collections) != 3 || cfg.Collections[0] != "core" || cfg.Collections[2] != "rolling" {
		t.Fatalf("unexpected collections: %v", cfg.Collections)
	}

	// Non-explicit missing file falls back to defaults.
	def, err := LoadKPMConfig(filepath.Join(dir, "missing.ini"), false)
	if err != nil {
		t.Fatalf("expected default fallback on missing non-explicit kpm.ini, got: %v", err)
	}
	if def.PortsDir != DefaultPortsDir || def.Root != "/" {
		t.Fatalf("unexpected default KPMConfig: %+v", def)
	}
}
