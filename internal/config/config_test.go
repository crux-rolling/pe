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
	if cfg.Repo.Repository != "kryx-linux/ports" || cfg.Repo.Tag != "rolling-x86_64" {
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
	examplePath := filepath.Join("..", "..", "kpm.ini.example")
	cfg, err := Load(examplePath, true)
	if err != nil {
		t.Fatalf("load kpm.ini.example: %v", err)
	}
	if cfg.Build.Jobs != 4 {
		t.Fatalf("expected jobs=4, got %d", cfg.Build.Jobs)
	}
	if cfg.Build.Compression != "zst" {
		t.Fatalf("expected compression=zst, got %q", cfg.Build.Compression)
	}
	wantCols := []string{"kpm-tools", "kernel", "system", "core", "opt", "xorg", "contrib", "compat-32"}
	if !reflect.DeepEqual(cfg.Paths.Collections, wantCols) {
		t.Fatalf("collections = %v, want %v", cfg.Paths.Collections, wantCols)
	}
}

func TestMultiRepoAndMultiPortsConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kpm.ini")
	content := `
[paths]
ports_dir = /usr/ports, /srv/local-ports
keys_dir = /etc/kpm/keys.d

[repo.rolling]
repository = kryx-linux/ports
tag = rolling-x86_64
verify_signature = true

[repo.extra]
index_url = https://example.org/extra/index.json
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, true)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantDirs := []string{"/usr/ports", "/srv/local-ports"}
	if !reflect.DeepEqual(cfg.EffectivePortsDirs(), wantDirs) {
		t.Fatalf("EffectivePortsDirs = %v, want %v", cfg.EffectivePortsDirs(), wantDirs)
	}
	repos := cfg.AllRepos()
	if len(repos) != 2 || repos[0].Name != "rolling" || !repos[0].VerifySignature || repos[1].Name != "extra" {
		t.Fatalf("unexpected AllRepos: %+v", repos)
	}
}

func TestParseCustomINI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kpm.ini")
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
repository = kryx-linux/ports
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
	wantURL := "https://github.com/kryx-linux/ports/releases/download/rolling-x86_64/index.json"
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
