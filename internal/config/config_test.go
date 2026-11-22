package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := Default()
	if cfg.Paths.Root != "/" {
		t.Fatalf("expected root /, got %q", cfg.Paths.Root)
	}
	if cfg.Paths.PortsDir != "/usr/ports" {
		t.Fatalf("expected ports_dir /usr/ports, got %q", cfg.Paths.PortsDir)
	}
	if cfg.Repo.Repository != "kryx-linux/packages" || cfg.Repo.Tag != "rolling-x86_64" {
		t.Fatalf("unexpected repo defaults: %+v", cfg.Repo)
	}
	if cfg.Build.Compression != "zst" {
		t.Fatalf("expected default compression zst, got %q", cfg.Build.Compression)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestParseINIAndExample(t *testing.T) {
	examplePath := filepath.Join("..", "..", "pe.ini.example")
	cfg, err := Load(examplePath, true)
	if err != nil {
		t.Fatalf("load pe.ini.example: %v", err)
	}
	if cfg.Build.Jobs != 4 {
		t.Fatalf("expected jobs=4, got %d", cfg.Build.Jobs)
	}
	if cfg.Build.Compression != "zst" {
		t.Fatalf("expected compression=zst, got %q", cfg.Build.Compression)
	}
	wantCols := []string{"core", "opt", "xorg", "contrib"}
	if !reflect.DeepEqual(cfg.Paths.Collections, wantCols) {
		t.Fatalf("collections = %v, want %v", cfg.Paths.Collections, wantCols)
	}
}

func TestParseCustomINI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pe.ini")
	content := `
# custom config
[paths]
root = /mnt/crux
ports_dir = /srv/ports
collections = core opt
source_cache = /srv/src
package_dir = /srv/pkg
work_dir = /srv/work

[repo]
repository = kryx-linux/packages
tag = rolling-x86_64
arch = x86_64
timeout_seconds = 30

[build]
jobs = 8
cflags = "-O3 -march=native -pipe"
compression = xz
ignore_footprint = yes
strip = false
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Paths.Root != "/mnt/crux" || cfg.Paths.SourceCache != "/srv/src" {
		t.Fatalf("unexpected paths: %+v", cfg.Paths)
	}
	if cfg.Build.Jobs != 8 || cfg.Build.MAKEFLAGS != "-j8" {
		t.Fatalf("unexpected jobs/makeflags: %d / %q", cfg.Build.Jobs, cfg.Build.MAKEFLAGS)
	}
	if cfg.Build.CFLAGS != "-O3 -march=native -pipe" || cfg.Build.CXXFLAGS != cfg.Build.CFLAGS {
		t.Fatalf("unexpected cflags/cxxflags: %q / %q", cfg.Build.CFLAGS, cfg.Build.CXXFLAGS)
	}
	if cfg.Build.Compression != "xz" || !cfg.Build.IgnoreFootprint || cfg.Build.Strip {
		t.Fatalf("unexpected build flags: %+v", cfg.Build)
	}
	wantURL := "https://github.com/kryx-linux/packages/releases/download/rolling-x86_64/index.json"
	if cfg.Repo.IndexURL != wantURL {
		t.Fatalf("IndexURL = %q, want %q", cfg.Repo.IndexURL, wantURL)
	}
}

func TestMissingNonExplicitConfigReturnsDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.ini"), false)
	if err != nil {
		t.Fatalf("expected defaults for missing non-explicit config, got %v", err)
	}
	if cfg.Build.Compression != "zst" {
		t.Fatalf("expected default zst, got %q", cfg.Build.Compression)
	}
}
