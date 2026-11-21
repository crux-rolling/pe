// Package config parses and validates pe's manual INI configuration file.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	DefaultConfigPath = "/etc/pe.ini"
	DefaultRepo       = "ezke5/packages"
	DefaultTag        = "rolling-x86_64"
	DefaultArch       = "x86_64"
	DefaultIndexURL   = "https://github.com/ezke5/packages/releases/download/rolling-x86_64/index.json"
)

// Config holds all runtime settings loaded from pe.ini.
type Config struct {
	Paths PathsConfig
	Repo  RepoConfig
	Build BuildConfig
	UI    UIConfig
}

// UIConfig configures terminal output behavior.
type UIConfig struct {
	Color string
}

// PathsConfig configures filesystem paths compatible with standard CRUX Linux.
type PathsConfig struct {
	Root            string
	PortsDir        string
	Collections     []string
	CacheDir        string
	SourceCache     string
	PackageDir      string
	WorkDir         string
	PkgaddConf      string
	PortsConfDir    string
	PortsDriversDir string
}

// RepoConfig configures the binary-first release index source.
type RepoConfig struct {
	Repository     string
	Tag            string
	Arch           string
	IndexURL       string
	TimeoutSeconds int
}

// BuildConfig configures Pkgfile builds (`pe build`), adapted from CRUX pkgmk.conf.
type BuildConfig struct {
	Jobs             int
	CFLAGS           string
	CXXFLAGS         string
	LDFLAGS          string
	MAKEFLAGS        string
	Compression      string
	SourceMirrors    []string
	DownloadSources  bool
	Strip            bool
	CompressManpages bool
	IgnoreFootprint  bool
	IgnoreNew        bool
	IgnoreMD5Sum     bool
	IgnoreSignature  bool
	KeepWork         bool
	DownloadProg     string
	CurlOpts         string
	WgetOpts         string
}

// Default returns a Config populated with standard CRUX-compatible defaults.
func Default() Config {
	jobs := runtime.NumCPU()
	if jobs < 1 {
		jobs = 1
	}
	return Config{
		Paths: PathsConfig{
			Root:            "/",
			PortsDir:        "/usr/ports",
			Collections:     []string{"core", "opt", "xorg", "contrib"},
			CacheDir:        "/var/cache/pe",
			SourceCache:     "/var/cache/pe/sources",
			PackageDir:      "/var/cache/pe/packages",
			WorkDir:         "/var/cache/pe/work",
			PkgaddConf:      "/etc/pkgadd.conf",
			PortsConfDir:    "/etc/ports",
			PortsDriversDir: "/etc/ports/drivers",
		},
		Repo: RepoConfig{
			Repository:     DefaultRepo,
			Tag:            DefaultTag,
			Arch:           DefaultArch,
			IndexURL:       DefaultIndexURL,
			TimeoutSeconds: 120,
		},
		Build: BuildConfig{
			Jobs:             jobs,
			CFLAGS:           "-O2 -march=x86-64 -pipe",
			CXXFLAGS:         "-O2 -march=x86-64 -pipe",
			LDFLAGS:          "",
			MAKEFLAGS:        fmt.Sprintf("-j%d", jobs),
			Compression:      "zst",
			DownloadSources:  true,
			Strip:            true,
			CompressManpages: true,
			IgnoreFootprint:  false,
			IgnoreNew:        false,
			IgnoreMD5Sum:     false,
			IgnoreSignature:  true,
			KeepWork:         false,
			DownloadProg:     "curl",
		},
		UI: UIConfig{
			Color: "auto",
		},
	}
}

// ResolvePath chooses the INI config file path from explicit flag, PE_CONFIG env, or default.
func ResolvePath(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("PE_CONFIG")); env != "" {
		return env
	}
	return DefaultConfigPath
}

// Load reads an INI configuration file layered over Default().
// If explicit is false and the file does not exist, defaults are returned with no error.
func Load(path string, explicit bool) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return cfg, nil
		}
		return cfg, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := Parse(string(data), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

// Parse parses INI text into cfg.
func Parse(input string, cfg *Config) error {
	scanner := bufio.NewScanner(strings.NewReader(input))
	section := ""
	lineNum := 0
	makeflagsSet := false
	cxxflagsSet := false
	indexURLSet := false

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.Index(line, "]")
			if end < 0 {
				return fmt.Errorf("line %d: unclosed section header %q", lineNum, line)
			}
			section = strings.ToLower(strings.TrimSpace(line[1:end]))
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			return fmt.Errorf("line %d: expected key = value, got %q", lineNum, line)
		}
		key := strings.ToLower(strings.TrimSpace(line[:eq]))
		val := stripInlineComment(strings.TrimSpace(line[eq+1:]))
		val = unquote(val)

		switch section {
		case "paths", "":
			switch key {
			case "root":
				cfg.Paths.Root = val
			case "ports_dir":
				cfg.Paths.PortsDir = val
			case "collections":
				cfg.Paths.Collections = splitList(val)
			case "cache_dir":
				cfg.Paths.CacheDir = val
			case "source_cache", "source_dir":
				cfg.Paths.SourceCache = val
			case "package_dir":
				cfg.Paths.PackageDir = val
			case "work_dir":
				cfg.Paths.WorkDir = val
			case "pkgadd_conf":
				cfg.Paths.PkgaddConf = val
			case "ports_conf_dir":
				cfg.Paths.PortsConfDir = val
			case "ports_drivers_dir":
				cfg.Paths.PortsDriversDir = val
			default:
				return fmt.Errorf("line %d: unknown key %q in [%s]", lineNum, key, section)
			}
		case "repo":
			switch key {
			case "repository":
				cfg.Repo.Repository = val
			case "tag":
				cfg.Repo.Tag = val
			case "arch":
				cfg.Repo.Arch = val
			case "index_url":
				cfg.Repo.IndexURL = val
				indexURLSet = true
			case "timeout_seconds":
				n, err := strconv.Atoi(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid timeout_seconds %q: %w", lineNum, val, err)
				}
				cfg.Repo.TimeoutSeconds = n
			default:
				return fmt.Errorf("line %d: unknown key %q in [repo]", lineNum, key)
			}
		case "build":
			switch key {
			case "jobs":
				n, err := strconv.Atoi(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid jobs %q: %w", lineNum, val, err)
				}
				cfg.Build.Jobs = n
			case "cflags":
				cfg.Build.CFLAGS = val
			case "cxxflags":
				cfg.Build.CXXFLAGS = val
				cxxflagsSet = true
			case "ldflags":
				cfg.Build.LDFLAGS = val
			case "makeflags":
				cfg.Build.MAKEFLAGS = val
				makeflagsSet = true
			case "compression":
				cfg.Build.Compression = strings.ToLower(val)
			case "source_mirrors":
				cfg.Build.SourceMirrors = splitList(val)
			case "download_sources":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid download_sources %q", lineNum, val)
				}
				cfg.Build.DownloadSources = b
			case "strip":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid strip %q", lineNum, val)
				}
				cfg.Build.Strip = b
			case "compress_manpages":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid compress_manpages %q", lineNum, val)
				}
				cfg.Build.CompressManpages = b
			case "ignore_footprint":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid ignore_footprint %q", lineNum, val)
				}
				cfg.Build.IgnoreFootprint = b
			case "ignore_new":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid ignore_new %q", lineNum, val)
				}
				cfg.Build.IgnoreNew = b
			case "ignore_md5sum":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid ignore_md5sum %q", lineNum, val)
				}
				cfg.Build.IgnoreMD5Sum = b
			case "ignore_signature":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid ignore_signature %q", lineNum, val)
				}
				cfg.Build.IgnoreSignature = b
			case "keep_work":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid keep_work %q", lineNum, val)
				}
				cfg.Build.KeepWork = b
			case "download_prog":
				cfg.Build.DownloadProg = val
			case "curl_opts":
				cfg.Build.CurlOpts = val
			case "wget_opts":
				cfg.Build.WgetOpts = val
			default:
				return fmt.Errorf("line %d: unknown key %q in [build]", lineNum, key)
			}
		case "ui":
			switch key {
			case "color":
				cfg.UI.Color = strings.ToLower(val)
			default:
				return fmt.Errorf("line %d: unknown key %q in [ui]", lineNum, key)
			}
		default:
			return fmt.Errorf("line %d: unknown section [%s]", lineNum, section)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if !cxxflagsSet {
		cfg.Build.CXXFLAGS = cfg.Build.CFLAGS
	}
	if !makeflagsSet && cfg.Build.Jobs > 0 {
		cfg.Build.MAKEFLAGS = fmt.Sprintf("-j%d", cfg.Build.Jobs)
	}
	if !indexURLSet && cfg.Repo.Repository != "" && cfg.Repo.Tag != "" {
		cfg.Repo.IndexURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/index.json", cfg.Repo.Repository, cfg.Repo.Tag)
	}
	return nil
}

// Validate verifies required config invariants.
func (c Config) Validate() error {
	if c.Build.Jobs < 1 {
		return fmt.Errorf("build.jobs must be >= 1, got %d", c.Build.Jobs)
	}
	switch c.Build.Compression {
	case "gz", "bz2", "xz", "lz", "zst":
	default:
		return fmt.Errorf("unsupported build.compression %q (expected gz, bz2, xz, lz, or zst)", c.Build.Compression)
	}
	if c.Repo.TimeoutSeconds < 1 {
		return fmt.Errorf("repo.timeout_seconds must be >= 1, got %d", c.Repo.TimeoutSeconds)
	}
	if c.Paths.Root == "" {
		return fmt.Errorf("paths.root must not be empty")
	}
	switch strings.ToLower(strings.TrimSpace(c.UI.Color)) {
	case "", "auto", "always", "never":
		if c.UI.Color == "" {
			c.UI.Color = "auto"
		}
	default:
		return fmt.Errorf("unsupported ui.color %q (expected auto, always, or never)", c.UI.Color)
	}
	return nil
}

// LocalIndexPath returns the local cached path for the synced index.json.
func (c Config) LocalIndexPath() string {
	return filepath.Join(c.Paths.CacheDir, "index.json")
}

// BinaryCacheDir returns the local directory where downloaded binary archives are cached.
func (c Config) BinaryCacheDir() string {
	return filepath.Join(c.Paths.CacheDir, "bin")
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "on":
		return true, nil
	case "false", "no", "0", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q", v)
	}
}

func stripInlineComment(s string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#', ';':
			if !inSingle && !inDouble && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return strings.TrimSpace(s)
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
