// Package config parses and validates kpm's manual INI configuration file.
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
	DefaultConfigPath = "/etc/kpm.ini"
	DefaultRepo       = "kryx-linux/ports"
	DefaultTag        = "rolling-x86_64"
	DefaultArch       = "x86_64"
	DefaultIndexURL   = "https://github.com/kryx-linux/ports/releases/download/rolling-x86_64/index.json"
)

// Config holds all runtime settings loaded from kpm.ini.
type Config struct {
	Paths PathsConfig
	Repo  RepoConfig
	Repos []NamedRepo // Optional multi-repo definitions from [repo.<name>] sections (in priority order)
	Build BuildConfig
	UI    UIConfig
}

// NamedRepo defines one named binary repository from a [repo.<name>] section.
type NamedRepo struct {
	Name            string
	Repository      string
	Tag             string
	Arch            string
	IndexURL        string
	TimeoutSeconds  int
	VerifySignature bool
	PubKey          string
}

// UIConfig configures terminal output behavior.
type UIConfig struct {
	Color string
}

// PathsConfig configures filesystem paths compatible with standard CRUX Linux.
type PathsConfig struct {
	Root            string
	PortsDir        string
	PortsDirs       []string // Optional priority-ordered multi-ports directories
	Collections     []string
	CacheDir        string
	SourceCache     string
	PackageDir      string
	WorkDir         string
	PkgaddConf      string
	KeysDir         string
	PortsConfDir    string
	PortsDriversDir string
}

// RepoConfig configures the binary-first release index source.
type RepoConfig struct {
	Repository      string
	Tag             string
	Arch            string
	IndexURL        string
	TimeoutSeconds  int
	VerifySignature bool
	PubKey          string
	Hold            []string
}

// BuildConfig configures Pkgfile builds (`kpm build`), adapted from CRUX pkgmk.conf.
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
			CacheDir:        "/var/cache/kpm",
			SourceCache:     "/var/cache/kpm/sources",
			PackageDir:      "/var/cache/kpm/packages",
			WorkDir:         "/var/cache/kpm/work",
			PkgaddConf:      "/etc/pkgadd.conf",
			KeysDir:         "/etc/kpm/keys.d",
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

// ResolvePath chooses the INI config file path from explicit flag, KPM_CONFIG env, or default.
func ResolvePath(explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	if env := strings.TrimSpace(os.Getenv("KPM_CONFIG")); env != "" {
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
	namedRepos := make(map[string]*NamedRepo)
	namedRepoOrder := make([]string, 0)
	namedIndexURLSet := make(map[string]bool)

	getOrCreateNamedRepo := func(name string) *NamedRepo {
		if nr, ok := namedRepos[name]; ok {
			return nr
		}
		nr := &NamedRepo{
			Name:            name,
			Arch:            cfg.Repo.Arch,
			TimeoutSeconds:  cfg.Repo.TimeoutSeconds,
			VerifySignature: cfg.Repo.VerifySignature,
			PubKey:          cfg.Repo.PubKey,
		}
		namedRepos[name] = nr
		namedRepoOrder = append(namedRepoOrder, name)
		return nr
	}

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
				dirs := splitPathList(val)
				if len(dirs) > 1 {
					cfg.Paths.PortsDir = dirs[0]
					cfg.Paths.PortsDirs = dirs
				} else {
					cfg.Paths.PortsDir = val
					cfg.Paths.PortsDirs = nil
				}
			case "ports_dirs":
				dirs := splitPathList(val)
				cfg.Paths.PortsDirs = dirs
				if len(dirs) > 0 {
					cfg.Paths.PortsDir = dirs[0]
				}
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
			case "keys_dir":
				cfg.Paths.KeysDir = val
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
			case "verify_signature", "verify_index_signature":
				b, err := parseBool(val)
				if err != nil {
					return fmt.Errorf("line %d: invalid %s %q", lineNum, key, val)
				}
				cfg.Repo.VerifySignature = b
			case "pubkey", "public_key":
				cfg.Repo.PubKey = val
			case "hold":
				cfg.Repo.Hold = splitList(val)
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
			if strings.HasPrefix(section, "repo.") && len(section) > len("repo.") {
				rname := strings.TrimSpace(strings.TrimPrefix(section, "repo."))
				nr := getOrCreateNamedRepo(rname)
				switch key {
				case "repository":
					nr.Repository = val
				case "tag":
					nr.Tag = val
				case "arch":
					nr.Arch = val
				case "index_url":
					nr.IndexURL = val
					namedIndexURLSet[rname] = true
				case "timeout_seconds":
					n, err := strconv.Atoi(val)
					if err != nil {
						return fmt.Errorf("line %d: invalid timeout_seconds %q: %w", lineNum, val, err)
					}
					nr.TimeoutSeconds = n
				case "verify_signature", "verify_index_signature":
					b, err := parseBool(val)
					if err != nil {
						return fmt.Errorf("line %d: invalid %s %q", lineNum, key, val)
					}
					nr.VerifySignature = b
				case "pubkey", "public_key":
					nr.PubKey = val
				default:
					return fmt.Errorf("line %d: unknown key %q in [%s]", lineNum, key, section)
				}
				continue
			}
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
	if len(namedRepoOrder) > 0 {
		cfg.Repos = make([]NamedRepo, 0, len(namedRepoOrder))
		for _, rname := range namedRepoOrder {
			nr := namedRepos[rname]
			if nr.Arch == "" {
				nr.Arch = cfg.Repo.Arch
			}
			if nr.TimeoutSeconds <= 0 {
				nr.TimeoutSeconds = cfg.Repo.TimeoutSeconds
			}
			if !namedIndexURLSet[rname] && nr.Repository != "" && nr.Tag != "" {
				nr.IndexURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/index.json", nr.Repository, nr.Tag)
			}
			cfg.Repos = append(cfg.Repos, *nr)
		}
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

// RepoIndexPath returns the local cache path for a named repository's index.json.
func (c Config) RepoIndexPath(repoName string) string {
	if repoName == "" || repoName == "default" {
		return c.LocalIndexPath()
	}
	return filepath.Join(c.Paths.CacheDir, "repos", repoName, "index.json")
}

// AllRepos returns the priority-ordered list of configured binary repositories.
// When [repo.<name>] sections exist, those are returned; otherwise the single [repo] section is used.
func (c Config) AllRepos() []NamedRepo {
	if len(c.Repos) > 0 {
		return c.Repos
	}
	return []NamedRepo{{
		Name:            "default",
		Repository:      c.Repo.Repository,
		Tag:             c.Repo.Tag,
		Arch:            c.Repo.Arch,
		IndexURL:        c.Repo.IndexURL,
		TimeoutSeconds:  c.Repo.TimeoutSeconds,
		VerifySignature: c.Repo.VerifySignature,
		PubKey:          c.Repo.PubKey,
	}}
}

// EffectivePortsDirs returns all configured local ports tree directories in priority order.
func (c Config) EffectivePortsDirs() []string {
	if len(c.Paths.PortsDirs) > 0 {
		return c.Paths.PortsDirs
	}
	if strings.TrimSpace(c.Paths.PortsDir) != "" {
		return []string{c.Paths.PortsDir}
	}
	return nil
}

// BinaryCacheDir returns the local directory where downloaded binary archives are cached.
func (c Config) BinaryCacheDir() string {
	return filepath.Join(c.Paths.CacheDir, "bin")
}

func splitPathList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ':'
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
