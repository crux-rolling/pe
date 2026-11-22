package srcmachine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kryx-linux/pe/internal/builder"
	"github.com/kryx-linux/pe/internal/config"
	"github.com/kryx-linux/pe/internal/pkgdb"
	"github.com/kryx-linux/pe/internal/pkgops"
	"github.com/kryx-linux/pe/internal/repo"
	"github.com/kryx-linux/pe/internal/ui"
)

// BuildOptions configures a unified kryx-src package build run.
type BuildOptions struct {
	PortsDir        string
	Collections     []string
	Targets         []string
	RepoHint        string // Optional owner/repo hint for single-port repos (e.g. kryx-linux/kernel)
	WorkRoot        string // Base isolated workspace for cache/sources/work
	OutDir          string // Directory where built .pkg.tar.<ext> archives are written
	Root            string // Target rootfs where build deps (and --install targets) are installed
	BaseConfigPath  string // Optional pe.ini base config
	Repository      string // Binary repo slug (e.g. kryx-linux/ports)
	ReleaseTag      string // Binary repo release tag (e.g. rolling-x86_64)
	Arch            string // Target arch (default x86_64)
	Jobs            int
	Compression     string
	CFLAGS          string
	CXXFLAGS        string
	SyncBinaryIndex bool // Fetch remote index.json before satisfying build deps
	WithDeps        bool
	SourceDeps      bool
	Force           bool
	Install         bool
	IgnoreFootprint bool
	IgnoreNew       bool
	UpdateFootprint bool
	IgnoreMD5Sum    bool
	IgnoreSignature bool
	KeepWork        bool
	NoStrip         bool
	DryRun          bool
	SkipRootCheck   bool
	SkipLdconfig    bool
	ColorMode       string
	Out             io.Writer
	ErrOut          io.Writer
}

// BuildResult summarizes all archives produced during a Build run.
type BuildResult struct {
	BuiltArchives []string
	BuiltPorts    []Entry
	MissingDeps   []string
}

// Build executes a unified xbps-src-style build pipeline using pe's builder,
// archive, pkgops, and repo internals with canonical Registry port resolution.
func Build(opts BuildOptions) (BuildResult, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.ErrOut == nil {
		opts.ErrOut = io.Discard
	}
	stOut := ui.NewStyler(opts.ColorMode, opts.Out)
	stErr := ui.NewStyler(opts.ColorMode, opts.ErrOut)

	cfg, err := buildConfig(opts)
	if err != nil {
		return BuildResult{}, err
	}

	for _, dir := range []string{cfg.Paths.CacheDir, cfg.Paths.SourceCache, cfg.Paths.PackageDir, cfg.Paths.WorkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return BuildResult{}, fmt.Errorf("create workspace dir %s: %w", dir, err)
		}
	}

	reg, err := LoadRegistry(cfg.Paths.PortsDir, cfg.Paths.Collections)
	if err != nil {
		return BuildResult{}, err
	}

	fmt.Fprintf(opts.Out, "%s discovered %d port(s) in %s\n",
		stOut.BoldCyan("==>"), len(reg.Entries), reg.PortsDir)

	roots, err := reg.ExpandTargets(opts.Targets, opts.RepoHint)
	if err != nil {
		return BuildResult{}, err
	}

	ordered, missing, err := reg.ResolveDepsOrder(roots, opts.WithDeps)
	if err != nil {
		return BuildResult{}, err
	}
	if len(missing) > 0 {
		fmt.Fprintf(opts.ErrOut, "%s dependencies not found in ports tree: %s\n",
			stErr.BoldYellow("kryx-src: warning:"), strings.Join(missing, ", "))
	}

	targetDirs := make(map[string]bool, len(roots))
	for _, r := range roots {
		targetDirs[r.Port.Dir] = true
	}

	// Print execution plan
	var planNames []string
	for _, e := range ordered {
		tag := "dep"
		if targetDirs[e.Port.Dir] {
			tag = "target"
		}
		planNames = append(planNames, fmt.Sprintf("%s (%s, %s)", e.Port.Name, e.RelDir, tag))
	}
	fmt.Fprintf(opts.Out, "%s build plan (%d): %s\n",
		stOut.BoldCyan("==>"), len(ordered), strings.Join(planNames, ", "))

	if opts.DryRun {
		return BuildResult{BuiltPorts: ordered, MissingDeps: missing}, nil
	}

	// Read currently installed packages under Root to skip already-installed build deps.
	installedMap := make(map[string]bool)
	if lock, lerr := pkgdb.AcquireLock(cfg.Paths.Root, false); lerr == nil {
		if db, derr := pkgdb.Open(cfg.Paths.Root); derr == nil {
			for name := range db.Packages {
				installedMap[name] = true
			}
		}
		_ = lock.Release()
	}

	// Load or sync remote binary index for binary-first dependency satisfaction.
	var binIdx repo.Index
	hasBinIdx := false
	if opts.WithDeps && !opts.SourceDeps {
		timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second
		if opts.SyncBinaryIndex && cfg.Repo.IndexURL != "" {
			if idx, serr := repo.SyncWithLog(cfg.Repo.IndexURL, cfg.LocalIndexPath(), timeout, opts.Out, stOut); serr == nil {
				binIdx = idx
				hasBinIdx = true
			} else {
				fmt.Fprintf(opts.ErrOut, "%s could not sync remote index.json (%v); falling back to source deps\n",
					stErr.BoldYellow("kryx-src: warning:"), serr)
			}
		} else if idx, lerr := repo.Load(cfg.LocalIndexPath()); lerr == nil {
			binIdx = idx
			hasBinIdx = true
		}
	}

	buildOpts := builder.Options{
		IgnoreFootprint: opts.IgnoreFootprint,
		IgnoreNew:       opts.IgnoreNew,
		UpdateFootprint: opts.UpdateFootprint,
		IgnoreMD5Sum:    opts.IgnoreMD5Sum,
		IgnoreSignature: opts.IgnoreSignature,
		KeepWork:        opts.KeepWork,
		NoStrip:         opts.NoStrip,
		Out:             opts.Out,
		ErrOut:          opts.ErrOut,
	}

	var result BuildResult
	result.MissingDeps = missing
	timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second

	for _, entry := range ordered {
		p := entry.Port
		isTarget := targetDirs[p.Dir]

		if !isTarget {
			if installedMap[p.Name] {
				fmt.Fprintf(opts.Out, "%s dependency %s already installed, skipping\n",
					stOut.BoldCyan("==>"), stOut.Bold(p.Name))
				continue
			}

			if hasBinIdx {
				if binEntry, ok := repo.Find(binIdx, p.Name, cfg.Repo.Arch); ok {
					fmt.Fprintf(opts.Out, "%s installing binary dependency %s %s\n",
						stOut.BoldCyan("==>"), stOut.Bold(binEntry.Pkgname), stOut.Cyan(binEntry.FullVersion()))
					localPath, ferr := repo.FetchBinaryWithStyle(binEntry, cfg.BinaryCacheDir(), timeout, opts.Out, stOut)
					if ferr != nil {
						return result, fmt.Errorf("fetch dependency %s: %w", p.Name, ferr)
					}
					name, ver, ierr := pkgops.InstallArchive(localPath, pkgops.InstallOptions{
						Root:          cfg.Paths.Root,
						PkgaddConf:    cfg.Paths.PkgaddConf,
						Reinstall:     true,
						Force:         opts.Force,
						SkipRootCheck: opts.SkipRootCheck,
						SkipLdconfig:  opts.SkipLdconfig,
						Out:           opts.Out,
						ErrOut:        opts.ErrOut,
					})
					if ierr != nil {
						return result, fmt.Errorf("install binary dependency %s: %w", p.Name, ierr)
					}
					installedMap[name] = true
					fmt.Fprintf(opts.Out, "%s dependency %s %s\n", stOut.BoldGreen("installed"), stOut.Bold(name), stOut.Cyan(ver))
					continue
				}
			}

			depOpts := buildOpts
			depOpts.Force = false
			res, berr := builder.Build(cfg, p, depOpts)
			if berr != nil {
				return result, fmt.Errorf("build dependency %s (%s): %w", p.Name, entry.RelDir, berr)
			}
			result.BuiltArchives = append(result.BuiltArchives, res.ArchivePath)
			result.BuiltPorts = append(result.BuiltPorts, entry)

			name, ver, ierr := pkgops.InstallArchive(res.ArchivePath, pkgops.InstallOptions{
				Root:          cfg.Paths.Root,
				PkgaddConf:    cfg.Paths.PkgaddConf,
				Reinstall:     true,
				Force:         opts.Force,
				SkipRootCheck: opts.SkipRootCheck,
				SkipLdconfig:  opts.SkipLdconfig,
				Out:           opts.Out,
				ErrOut:        opts.ErrOut,
			})
			if ierr != nil {
				return result, fmt.Errorf("install built dependency %s: %w", p.Name, ierr)
			}
			installedMap[name] = true
			fmt.Fprintf(opts.Out, "%s dependency %s %s\n", stOut.BoldGreen("installed"), stOut.Bold(name), stOut.Cyan(ver))
			continue
		}

		// Explicitly requested target port: always built from source.
		targetOpts := buildOpts
		targetOpts.Force = opts.Force
		res, berr := builder.Build(cfg, p, targetOpts)
		if berr != nil {
			return result, fmt.Errorf("build target %s (%s): %w", p.Name, entry.RelDir, berr)
		}
		result.BuiltArchives = append(result.BuiltArchives, res.ArchivePath)
		result.BuiltPorts = append(result.BuiltPorts, entry)

		if opts.Install {
			name, ver, ierr := pkgops.InstallArchive(res.ArchivePath, pkgops.InstallOptions{
				Root:          cfg.Paths.Root,
				PkgaddConf:    cfg.Paths.PkgaddConf,
				Reinstall:     true,
				Force:         opts.Force,
				SkipRootCheck: opts.SkipRootCheck,
				SkipLdconfig:  opts.SkipLdconfig,
				Out:           opts.Out,
				ErrOut:        opts.ErrOut,
			})
			if ierr != nil {
				return result, fmt.Errorf("install target %s: %w", p.Name, ierr)
			}
			installedMap[name] = true
			fmt.Fprintf(opts.Out, "%s %s %s\n", stOut.BoldGreen("installed"), stOut.Bold(name), stOut.Cyan(ver))
		}
	}

	return result, nil
}

func buildConfig(opts BuildOptions) (config.Config, error) {
	cfg := config.Default()
	if strings.TrimSpace(opts.BaseConfigPath) != "" {
		loaded, err := config.Load(opts.BaseConfigPath, true)
		if err != nil {
			return cfg, err
		}
		cfg = loaded
	}

	if opts.Root != "" {
		cfg.Paths.Root = opts.Root
	}
	if opts.PortsDir != "" {
		cfg.Paths.PortsDir = opts.PortsDir
	}
	// Empty Collections means auto-discover all collections in PortsDir.
	cfg.Paths.Collections = opts.Collections

	if opts.WorkRoot != "" {
		absWork, err := filepath.Abs(opts.WorkRoot)
		if err != nil {
			return cfg, fmt.Errorf("resolve work root: %w", err)
		}
		cfg.Paths.CacheDir = filepath.Join(absWork, "cache")
		cfg.Paths.SourceCache = filepath.Join(absWork, "sources")
		cfg.Paths.WorkDir = filepath.Join(absWork, "work")
		cfg.Paths.PackageDir = filepath.Join(absWork, "packages")
	}
	if opts.OutDir != "" {
		absOut, err := filepath.Abs(opts.OutDir)
		if err != nil {
			return cfg, fmt.Errorf("resolve out dir: %w", err)
		}
		cfg.Paths.PackageDir = absOut
	}

	if opts.Repository != "" {
		cfg.Repo.Repository = opts.Repository
	}
	if opts.ReleaseTag != "" {
		cfg.Repo.Tag = opts.ReleaseTag
	}
	if opts.Arch != "" {
		cfg.Repo.Arch = opts.Arch
	}
	if cfg.Repo.Repository != "" && cfg.Repo.Tag != "" {
		cfg.Repo.IndexURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/index.json", cfg.Repo.Repository, cfg.Repo.Tag)
	}
	if cfg.Repo.TimeoutSeconds < 600 {
		cfg.Repo.TimeoutSeconds = 600
	}

	if opts.Jobs > 0 {
		cfg.Build.Jobs = opts.Jobs
	} else if cfg.Build.Jobs <= 0 {
		cfg.Build.Jobs = runtime.NumCPU()
	}
	cfg.Build.MAKEFLAGS = fmt.Sprintf("-j%d", cfg.Build.Jobs)

	if opts.Compression != "" {
		cfg.Build.Compression = strings.ToLower(opts.Compression)
	}
	if opts.CFLAGS != "" {
		cfg.Build.CFLAGS = opts.CFLAGS
	}
	if opts.CXXFLAGS != "" {
		cfg.Build.CXXFLAGS = opts.CXXFLAGS
	}
	if opts.ColorMode != "" {
		cfg.UI.Color = opts.ColorMode
	}

	cfg.Build.IgnoreSignature = true
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}
