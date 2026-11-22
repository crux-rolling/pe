// Package cli implements the unified pe command-line interface for CRUX Linux:
// sync, update, install, build, search, list, and remove/uninstall.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ezke5/pe/internal/archive"
	"github.com/ezke5/pe/internal/builder"
	"github.com/ezke5/pe/internal/config"
	"github.com/ezke5/pe/internal/pkgdb"
	"github.com/ezke5/pe/internal/pkgops"
	"github.com/ezke5/pe/internal/ports"
	"github.com/ezke5/pe/internal/repo"
	"github.com/ezke5/pe/internal/ui"
)

// Version is injected at link time via -ldflags.
var Version = "0.1.0"

// RuntimeOptions allows tests to run package transactions against temporary roots
// without requiring root privileges or running host ldconfig.
type RuntimeOptions struct {
	SkipRootCheck bool
	SkipLdconfig  bool
}

type uiContext struct {
	out ui.Styler
	err ui.Styler
}

// Run executes the pe CLI with standard production runtime options.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithOptions(args, stdout, stderr, RuntimeOptions{})
}

// RunWithOptions executes the pe CLI with explicit runtime options.
func RunWithOptions(args []string, stdout, stderr io.Writer, rt RuntimeOptions) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	globalFlags, rest, err := parseGlobalArgs(args)
	if err != nil {
		es := ui.NewStyler(ui.ColorAuto, stderr)
		fmt.Fprintf(stderr, "%s %v\n", es.BoldRed("pe:"), err)
		return 2
	}
	if globalFlags.showVersion {
		fmt.Fprintf(stdout, "pe %s\n", Version)
		return 0
	}
	if globalFlags.showHelp || len(rest) == 0 {
		printUsage(stdout)
		if len(rest) == 0 && !globalFlags.showHelp {
			return 2
		}
		return 0
	}

	cfgPath := config.ResolvePath(globalFlags.configPath)
	explicitCfg := strings.TrimSpace(globalFlags.configPath) != "" || strings.TrimSpace(os.Getenv("PE_CONFIG")) != ""
	cfg, err := config.Load(cfgPath, explicitCfg)
	if err != nil {
		es := ui.NewStyler(globalFlags.colorMode, stderr)
		fmt.Fprintf(stderr, "%s %v\n", es.BoldRed("pe:"), err)
		return 1
	}
	if strings.TrimSpace(globalFlags.rootOverride) != "" {
		cfg.Paths.Root = globalFlags.rootOverride
	}
	if globalFlags.colorMode != "" {
		cfg.UI.Color = globalFlags.colorMode
	}

	styles := uiContext{
		out: ui.NewStyler(cfg.UI.Color, stdout),
		err: ui.NewStyler(cfg.UI.Color, stderr),
	}

	cmd := rest[0]
	cmdArgs := rest[1:]

	var runErr error
	switch cmd {
	case "sync":
		runErr = cmdSync(cfg, cmdArgs, stdout, stderr, styles)
	case "update":
		runErr = cmdUpdate(cfg, cmdArgs, stdout, stderr, rt, styles)
	case "install":
		runErr = cmdInstall(cfg, cmdArgs, stdout, stderr, rt, styles)
	case "build":
		runErr = cmdBuild(cfg, cmdArgs, stdout, stderr, rt, styles)
	case "search":
		runErr = cmdSearch(cfg, cmdArgs, stdout, stderr, styles)
	case "list":
		runErr = cmdList(cfg, cmdArgs, stdout, stderr, styles)
	case "remove", "uninstall", "rm":
		runErr = cmdRemove(cfg, cmdArgs, stdout, stderr, rt, styles)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "pe %s\n", Version)
		return 0
	default:
		fmt.Fprintf(stderr, "%s unknown command %q\n\n", styles.err.BoldRed("pe:"), cmd)
		printUsage(stderr)
		return 2
	}

	if runErr != nil {
		fmt.Fprintf(stderr, "%s %v\n", styles.err.BoldRed("pe:"), runErr)
		return 1
	}
	return 0
}

type globalOpts struct {
	configPath   string
	rootOverride string
	colorMode    string
	showHelp     bool
	showVersion  bool
}

func parseGlobalArgs(args []string) (globalOpts, []string, error) {
	var opts globalOpts
	i := 0
	for i < len(args) {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		switch {
		case a == "-h" || a == "--help":
			opts.showHelp = true
			i++
		case a == "-v" || a == "--version":
			opts.showVersion = true
			i++
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("option %s requires an argument", a)
			}
			opts.configPath = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--config="):
			opts.configPath = strings.TrimPrefix(a, "--config=")
			i++
		case a == "-r" || a == "--root":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("option %s requires an argument", a)
			}
			opts.rootOverride = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--root="):
			opts.rootOverride = strings.TrimPrefix(a, "--root=")
			i++
		case a == "--no-color":
			opts.colorMode = ui.ColorNever
			i++
		case a == "--color":
			if i+1 >= len(args) {
				return opts, nil, fmt.Errorf("option %s requires an argument (auto, always, or never)", a)
			}
			mode, err := ui.NormalizeColorMode(args[i+1])
			if err != nil {
				return opts, nil, err
			}
			opts.colorMode = mode
			i += 2
		case strings.HasPrefix(a, "--color="):
			mode, err := ui.NormalizeColorMode(strings.TrimPrefix(a, "--color="))
			if err != nil {
				return opts, nil, err
			}
			opts.colorMode = mode
			i++
		default:
			return opts, nil, fmt.Errorf("unknown global option %q", a)
		}
	}
	return opts, args[i:], nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: pe [global options] <command> [options] [args...]

Global options:
  -c, --config <file>     path to pe.ini configuration file (default: /etc/pe.ini)
  -r, --root <path>       specify alternative CRUX installation root
      --color <mode>      colorize output: auto, always, or never (default: auto)
      --no-color          disable colored output (same as --color=never)
  -v, --version           print version and exit
  -h, --help              print help and exit

Commands:
  sync                    sync binary index.json from ezke5/packages rolling-x86_64 (and optional --ports)
  update                  upgrade installed packages binary-first (reports missing binaries for pe build)
  install                 install binary package(s) from index.json or local .pkg.tar.<ext> archive
  build                   build package(s) from local CRUX Pkgfile ports using pe.ini settings
  search                  short-form search across binary index, local ports, and installed packages
  list                    list installed packages, ports (--ports), binaries (--binaries), files, or owners
  remove, uninstall       remove installed package(s) using CRUX pkgrm policy
`)
}

// --- sync ---

func cmdSync(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	syncPorts := fs.Bool("ports", false, "also sync CRUX ports collections via /etc/ports/drivers")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second
	idx, err := repo.SyncWithLog(cfg.Repo.IndexURL, cfg.LocalIndexPath(), timeout, stdout, styles.out)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w (run with sudo or configure cache_dir in pe.ini)", err)
		}
		return err
	}
	fmt.Fprintf(stdout, "%s %s (%d packages) from %s (%s)\n",
		styles.out.BoldGreen("synced"),
		cfg.LocalIndexPath(),
		len(idx.Packages),
		styles.out.Bold(cfg.Repo.Repository),
		styles.out.Cyan(cfg.Repo.Tag),
	)

	if *syncPorts {
		cols := fs.Args()
		if err := ports.SyncPorts(cfg.Paths.PortsDir, cfg.Paths.PortsConfDir, cfg.Paths.PortsDriversDir, cols, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

// --- install (strictly binary-first, never auto-builds) ---

func cmdInstall(cfg config.Config, args []string, stdout, stderr io.Writer, rt RuntimeOptions, styles uiContext) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	upgrade := fs.Bool("upgrade", false, "upgrade an already installed package")
	upgradeShort := fs.Bool("u", false, "upgrade an already installed package")
	force := fs.Bool("force", false, "overwrite conflicting files")
	forceShort := fs.Bool("f", false, "overwrite conflicting files")
	verbose := fs.Bool("verbose", false, "list all conflicting files without truncation")
	withDeps := fs.Bool("deps", false, "also install missing dependencies from binary index")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	targets := fs.Args()
	if len(targets) == 0 {
		return fmt.Errorf("usage: pe install [--upgrade] [--force] [--deps] [--verbose] <package|file...>")
	}

	doUpgrade := *upgrade || *upgradeShort
	doForce := *force || *forceShort
	doVerbose := *verbose

	var localFiles []string
	var pkgNames []string
	for _, t := range targets {
		if _, _, _, ok := archive.ParsePkgFilename(t); ok {
			if _, err := os.Stat(t); err == nil {
				localFiles = append(localFiles, t)
				continue
			}
		}
		pkgNames = append(pkgNames, t)
	}

	// Install explicit local .pkg.tar.<ext> files directly using CRUX pkgadd policy.
	for _, file := range localFiles {
		name, ver, err := pkgops.InstallArchive(file, pkgops.InstallOptions{
			Root:          cfg.Paths.Root,
			PkgaddConf:    cfg.Paths.PkgaddConf,
			Upgrade:       doUpgrade,
			Force:         doForce,
			Verbose:       doVerbose,
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			Out:           stdout,
			ErrOut:        stderr,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s %s\n", styles.out.BoldGreen("installed"), styles.out.Bold(name), styles.out.Cyan(ver))
	}

	if len(pkgNames) == 0 {
		return nil
	}

	// Load synced binary index (strictly binary-first).
	idx, err := loadLocalIndex(cfg)
	if err != nil {
		return err
	}

	// Read currently installed packages if resolving dependencies.
	var installedMap map[string]pkgdb.PkgInfo
	if *withDeps {
		if db, dbErr := openDBReadOnly(cfg.Paths.Root); dbErr == nil {
			installedMap = db.Packages
		}
	}

	installPlan := pkgNames
	if *withDeps {
		installPlan = resolveBinaryDeps(cfg, idx, pkgNames, installedMap)
	}

	// Verify all requested packages exist in binary index before modifying the system.
	var missing []string
	entries := make([]repo.Entry, 0, len(installPlan))
	for _, name := range installPlan {
		entry, ok := repo.Find(idx, name, cfg.Repo.Arch)
		if !ok {
			missing = append(missing, name)
			continue
		}
		entries = append(entries, entry)
	}
	if len(missing) > 0 {
		for _, m := range missing {
			fmt.Fprintf(stderr, "%s no binary available for %s in %s (%s); run: %s\n",
				styles.err.BoldYellow("warning:"),
				styles.err.Bold(m),
				cfg.Repo.Repository,
				cfg.Repo.Tag,
				styles.err.Bold("pe build "+m),
			)
		}
		return fmt.Errorf("binary package(s) unavailable: %s (run `pe build <pkg>` to compile from ports)", strings.Join(missing, ", "))
	}

	timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second
	for _, entry := range entries {
		localPath, err := repo.FetchBinaryWithStyle(entry, cfg.BinaryCacheDir(), timeout, stdout, styles.out)
		if err != nil {
			return err
		}
		name, ver, err := pkgops.InstallArchive(localPath, pkgops.InstallOptions{
			Root:          cfg.Paths.Root,
			PkgaddConf:    cfg.Paths.PkgaddConf,
			Upgrade:       doUpgrade,
			Force:         doForce,
			Verbose:       doVerbose,
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			Out:           stdout,
			ErrOut:        stderr,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s %s\n", styles.out.BoldGreen("installed"), styles.out.Bold(name), styles.out.Cyan(ver))
	}
	return nil
}

// --- update (binary-first upgrade of installed packages) ---

func cmdUpdate(cfg config.Config, args []string, stdout, stderr io.Writer, rt RuntimeOptions, styles uiContext) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "overwrite conflicting files during upgrade")
	forceShort := fs.Bool("f", false, "overwrite conflicting files during upgrade")
	verbose := fs.Bool("verbose", false, "list all conflicting files without truncation")
	noSync := fs.Bool("no-sync", false, "use cached index.json without syncing first")
	dryRun := fs.Bool("dry-run", false, "show available updates without installing")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	var idx repo.Index
	var err error
	if *noSync {
		idx, err = loadLocalIndex(cfg)
	} else {
		timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second
		idx, err = repo.SyncWithLog(cfg.Repo.IndexURL, cfg.LocalIndexPath(), timeout, stdout, styles.out)
		if err != nil && errors.Is(err, os.ErrPermission) {
			err = fmt.Errorf("%w (run with sudo or configure cache_dir in pe.ini)", err)
		}
	}
	if err != nil {
		return err
	}

	db, err := openDBReadOnly(cfg.Paths.Root)
	if err != nil {
		return err
	}

	targets := fs.Args()
	if len(targets) == 0 {
		targets = db.SortedNames()
	} else {
		for _, t := range targets {
			if !db.FindPkg(t) {
				return fmt.Errorf("package %s is not installed", t)
			}
		}
	}

	// Optional local ports lookup so we can alert the user if a port is newer but lacks a binary.
	portMap := discoverPortMap(cfg)

	var toUpgrade []repo.Entry
	var needsBuild []string

	for _, name := range targets {
		instVer := db.Packages[name].Version
		if entry, ok := repo.Find(idx, name, cfg.Repo.Arch); ok {
			if entry.FullVersion() != instVer {
				toUpgrade = append(toUpgrade, entry)
			}
			continue
		}
		if p, ok := portMap[name]; ok && p.FullVersion() != instVer {
			needsBuild = append(needsBuild, name)
			fmt.Fprintf(stderr, "%s no binary available for %s (%s -> %s) in %s (%s); run: %s\n",
				styles.err.BoldYellow("warning:"),
				styles.err.Bold(name),
				instVer,
				p.FullVersion(),
				cfg.Repo.Repository,
				cfg.Repo.Tag,
				styles.err.Bold("pe build "+name),
			)
		}
	}

	if len(toUpgrade) == 0 {
		if len(needsBuild) == 0 {
			fmt.Fprintln(stdout, styles.out.Green("all packages are up to date"))
		}
		return nil
	}

	if *dryRun {
		for _, e := range toUpgrade {
			fmt.Fprintf(stdout, "%s %s -> %s\n",
				styles.out.Bold(e.Pkgname),
				styles.out.Yellow(db.Packages[e.Pkgname].Version),
				styles.out.Green(e.FullVersion()),
			)
		}
		return nil
	}

	doForce := *force || *forceShort
	timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second
	for _, entry := range toUpgrade {
		oldVer := db.Packages[entry.Pkgname].Version
		localPath, err := repo.FetchBinaryWithStyle(entry, cfg.BinaryCacheDir(), timeout, stdout, styles.out)
		if err != nil {
			return err
		}
		name, newVer, err := pkgops.InstallArchive(localPath, pkgops.InstallOptions{
			Root:          cfg.Paths.Root,
			PkgaddConf:    cfg.Paths.PkgaddConf,
			Upgrade:       true,
			Force:         doForce,
			Verbose:       *verbose,
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			Out:           stdout,
			ErrOut:        stderr,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s %s -> %s\n",
			styles.out.BoldGreen("updated"),
			styles.out.Bold(name),
			styles.out.Yellow(oldVer),
			styles.out.Green(newVer),
		)
	}
	return nil
}

// --- build (explicit Pkgfile compilation via pe.ini) ---

func cmdBuild(cfg config.Config, args []string, stdout, stderr io.Writer, rt RuntimeOptions, styles uiContext) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	install := fs.Bool("install", false, "install built package after building")
	installShort := fs.Bool("i", false, "install built package after building")
	upgrade := fs.Bool("upgrade", false, "upgrade installed package after building")
	upgradeShort := fs.Bool("u", false, "upgrade installed package after building")
	force := fs.Bool("force", false, "force rebuild even if archive is up to date")
	forceShort := fs.Bool("f", false, "force rebuild even if archive is up to date")
	withDeps := fs.Bool("deps", false, "build recursive Pkgfile dependencies first")
	ignoreFP := fs.Bool("ignore-footprint", false, "skip footprint verification")
	ignoreNew := fs.Bool("ignore-new", false, "allow new files in footprint check")
	updateFP := fs.Bool("update-footprint", false, "update .footprint from build output")
	ignoreMD5 := fs.Bool("ignore-md5sum", false, "skip .md5sum verification")
	ignoreSig := fs.Bool("ignore-signature", false, "skip .signature verification")
	keepWork := fs.Bool("keep-work", false, "keep temporary work directory")
	noStrip := fs.Bool("no-strip", false, "do not strip ELF binaries")
	verbose := fs.Bool("verbose", false, "list all conflicting files without truncation")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	targets := fs.Args()
	if len(targets) == 0 {
		return fmt.Errorf("usage: pe build [options] <port...>")
	}

	// Expand targets: each target may be a single port (found recursively at any depth)
	// or a directory/collection under ports_dir containing nested Pkgfiles.
	var expandedTargets []string
	var directPorts []ports.Port
	for _, t := range targets {
		p, err := ports.Find(cfg.Paths.PortsDir, cfg.Paths.Collections, t)
		if err == nil {
			expandedTargets = append(expandedTargets, p.QualifiedName())
			directPorts = append(directPorts, p)
			continue
		}
		group, gerr := ports.FindGroup(cfg.Paths.PortsDir, t)
		if gerr != nil {
			return gerr
		}
		if len(group) > 0 {
			for _, gp := range group {
				expandedTargets = append(expandedTargets, gp.QualifiedName())
				directPorts = append(directPorts, gp)
			}
			continue
		}
		return err
	}

	var buildList []ports.Port
	if *withDeps {
		ordered, missing, err := ports.ResolveDeps(cfg.Paths.PortsDir, cfg.Paths.Collections, expandedTargets)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			fmt.Fprintf(stderr, "%s dependencies not found in ports tree: %s\n",
				styles.err.BoldYellow("pe: warning:"), strings.Join(missing, ", "))
		}
		buildList = ordered
	} else {
		buildList = directPorts
	}

	doInstall := *install || *installShort
	doUpgrade := *upgrade || *upgradeShort
	doForce := *force || *forceShort

	for _, p := range buildList {
		res, err := builder.Build(cfg, p, builder.Options{
			Force:           doForce,
			IgnoreFootprint: *ignoreFP,
			IgnoreNew:       *ignoreNew,
			UpdateFootprint: *updateFP,
			IgnoreMD5Sum:    *ignoreMD5,
			IgnoreSignature: *ignoreSig,
			KeepWork:        *keepWork,
			NoStrip:         *noStrip,
			Out:             stdout,
			ErrOut:          stderr,
		})
		if err != nil {
			return err
		}
		if doInstall || doUpgrade {
			isUpgrade := doUpgrade
			if !isUpgrade {
				if db, err := openDBReadOnly(cfg.Paths.Root); err == nil && db.FindPkg(p.Name) {
					isUpgrade = true
				}
			}
			name, ver, err := pkgops.InstallArchive(res.ArchivePath, pkgops.InstallOptions{
				Root:          cfg.Paths.Root,
				PkgaddConf:    cfg.Paths.PkgaddConf,
				Upgrade:       isUpgrade,
				Force:         doForce,
				Verbose:       *verbose,
				SkipRootCheck: rt.SkipRootCheck,
				SkipLdconfig:  rt.SkipLdconfig,
				Out:           stdout,
				ErrOut:        stderr,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s %s %s\n", styles.out.BoldGreen("installed"), styles.out.Bold(name), styles.out.Cyan(ver))
		}
	}
	return nil
}

// --- search (short-form output) ---

type searchRecord struct {
	name        string
	version     string
	description string
	installed   bool
	binary      bool
	port        bool
}

func cmdSearch(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	onlyInstalled := fs.Bool("installed", false, "search only installed packages")
	onlyPorts := fs.Bool("ports", false, "search only local ports tree")
	onlyBinaries := fs.Bool("binaries", false, "search only binary repository index")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: pe search [--installed|--ports|--binaries] <query>")
	}
	query := strings.ToLower(strings.Join(fs.Args(), " "))

	includeAll := !*onlyInstalled && !*onlyPorts && !*onlyBinaries
	records := make(map[string]*searchRecord)
	getRec := func(name string) *searchRecord {
		r, ok := records[name]
		if !ok {
			r = &searchRecord{name: name}
			records[name] = r
		}
		return r
	}

	// 1. Binary index
	if includeAll || *onlyBinaries {
		idxPath := cfg.LocalIndexPath()
		idx, err := repo.Load(idxPath)
		if err == nil {
			for _, name := range idx.SortedNames() {
				entry, ok := repo.Find(idx, name, cfg.Repo.Arch)
				if !ok {
					continue
				}
				r := getRec(name)
				r.binary = true
				if r.version == "" {
					r.version = entry.FullVersion()
				}
				if r.description == "" {
					r.description = entry.Description
				}
			}
		} else if errors.Is(err, os.ErrPermission) {
			fmt.Fprintf(stderr, "%s cannot read %s: permission denied (run `sudo pe sync` to refresh permissions)\n",
				styles.err.BoldYellow("pe: warning:"), idxPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "%s cannot load %s: %v\n", styles.err.BoldYellow("pe: warning:"), idxPath, err)
		} else if *onlyBinaries {
			return fmt.Errorf("local index %s not found; run `pe sync` first", idxPath)
		}
	}

	// 2. Local ports tree
	if includeAll || *onlyPorts {
		if allPorts, err := ports.DiscoverAll(cfg.Paths.PortsDir, cfg.Paths.Collections); err == nil {
			for _, p := range allPorts {
				r := getRec(p.Name)
				r.port = true
				if r.version == "" {
					r.version = p.FullVersion()
				}
				if r.description == "" {
					r.description = p.Description
				}
			}
		} else if *onlyPorts {
			return err
		}
	}

	// 3. Installed DB
	if includeAll || *onlyInstalled {
		if db, err := openDBReadOnly(cfg.Paths.Root); err == nil {
			for _, name := range db.SortedNames() {
				r := getRec(name)
				r.installed = true
				if r.version == "" {
					r.version = db.Packages[name].Version
				}
			}
		} else if *onlyInstalled {
			return err
		}
	}

	names := make([]string, 0, len(records))
	for name, r := range records {
		if *onlyInstalled && !r.installed {
			continue
		}
		if *onlyPorts && !r.port {
			continue
		}
		if *onlyBinaries && !r.binary {
			continue
		}
		if strings.Contains(strings.ToLower(name), query) || strings.Contains(strings.ToLower(r.description), query) {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	if len(names) == 0 {
		return nil
	}

	// Short-form output: one concise line per match.
	for _, name := range names {
		r := records[name]
		var tags []string
		if r.installed {
			tags = append(tags, styles.out.Green("installed"))
		}
		if r.binary {
			tags = append(tags, styles.out.Cyan("bin"))
		}
		if r.port {
			tags = append(tags, styles.out.Yellow("port"))
		}
		status := "[" + strings.Join(tags, ",") + "]"
		if r.description != "" {
			fmt.Fprintf(stdout, "%s %s %s %s\n",
				styles.out.Bold(r.name),
				styles.out.Cyan(r.version),
				status,
				styles.out.Dim("- "+r.description),
			)
		} else {
			fmt.Fprintf(stdout, "%s %s %s\n",
				styles.out.Bold(r.name),
				styles.out.Cyan(r.version),
				status,
			)
		}
	}
	return nil
}

// --- list (installed, ports, binaries, files, owner, diff) ---

func cmdList(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listPorts := fs.Bool("ports", false, "list available ports in the local ports tree (CRUX ports -l)")
	listBinaries := fs.Bool("binaries", false, "list available binary packages from synced index.json")
	listFiles := fs.String("files", "", "list files owned by installed <package> or local <archive>")
	ownerPattern := fs.String("owner", "", "list package(s) owning files matching <regex>")
	listDiff := fs.Bool("diff", false, "list version differences between installed packages and binaries/ports")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	modeCount := 0
	if *listPorts {
		modeCount++
	}
	if *listBinaries {
		modeCount++
	}
	if *listFiles != "" {
		modeCount++
	}
	if *ownerPattern != "" {
		modeCount++
	}
	if *listDiff {
		modeCount++
	}
	if modeCount > 1 {
		return fmt.Errorf("specify at most one mode flag for list")
	}

	switch {
	case *listPorts:
		cols := cfg.Paths.Collections
		if fs.NArg() > 0 {
			cols = fs.Args()
		}
		entries, err := ports.ListEntries(cfg.Paths.PortsDir, cols)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if idx := strings.LastIndex(e, "/"); idx > 0 && styles.out.Enabled {
				fmt.Fprintf(stdout, "%s/%s\n", styles.out.Dim(e[:idx]), styles.out.Bold(e[idx+1:]))
			} else {
				fmt.Fprintln(stdout, e)
			}
		}
		return nil

	case *listBinaries:
		idx, err := loadLocalIndex(cfg)
		if err != nil {
			return err
		}
		for _, name := range idx.SortedNames() {
			if e, ok := repo.Find(idx, name, cfg.Repo.Arch); ok {
				fmt.Fprintf(stdout, "%s %s\n", styles.out.Bold(e.Pkgname), styles.out.Cyan(e.FullVersion()))
			}
		}
		return nil

	case *listFiles != "":
		target := *listFiles
		if db, err := openDBReadOnly(cfg.Paths.Root); err == nil && db.FindPkg(target) {
			for _, f := range db.Packages[target].SortedFiles() {
				fmt.Fprintln(stdout, f)
			}
			return nil
		}
		if _, err := os.Stat(target); err == nil {
			_, info, err := archive.Open(target)
			if err != nil {
				return err
			}
			for _, f := range info.SortedFiles() {
				fmt.Fprintln(stdout, f)
			}
			return nil
		}
		return fmt.Errorf("%s is neither an installed package nor a package file", target)

	case *ownerPattern != "":
		re, err := regexp.Compile(*ownerPattern)
		if err != nil {
			return fmt.Errorf("error compiling regular expression %q: %w", *ownerPattern, err)
		}
		db, err := openDBReadOnly(cfg.Paths.Root)
		if err != nil {
			return err
		}
		type match struct{ pkg, file string }
		var matches []match
		for _, name := range db.SortedNames() {
			for _, f := range db.Packages[name].SortedFiles() {
				if re.MatchString("/" + f) {
					matches = append(matches, match{name, f})
				}
			}
		}
		if len(matches) == 0 {
			fmt.Fprintln(stdout, "pe: no owner(s) found")
			return nil
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\n", styles.out.Bold("Package"), styles.out.Bold("File"))
		for _, m := range matches {
			fmt.Fprintf(tw, "%s\t%s\n", styles.out.Bold(m.pkg), m.file)
		}
		return tw.Flush()

	case *listDiff:
		db, err := openDBReadOnly(cfg.Paths.Root)
		if err != nil {
			return err
		}
		idx, _ := repo.Load(cfg.LocalIndexPath())
		portMap := discoverPortMap(cfg)

		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		found := false
		for _, name := range db.SortedNames() {
			instVer := db.Packages[name].Version
			var source, availVer string
			if e, ok := repo.Find(idx, name, cfg.Repo.Arch); ok {
				source = "binary"
				availVer = e.FullVersion()
			} else if p, ok := portMap[name]; ok {
				source = "port"
				if p.Collection != "" {
					source = p.Collection
				}
				availVer = p.FullVersion()
			} else {
				continue
			}
			if availVer != instVer {
				if !found {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
						styles.out.Bold("Source"),
						styles.out.Bold("Name"),
						styles.out.Bold("Available"),
						styles.out.Bold("Installed"),
					)
					found = true
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
					styles.out.Cyan(source),
					styles.out.Bold(name),
					styles.out.Green(availVer),
					styles.out.Yellow(instVer),
				)
			}
		}
		if !found {
			fmt.Fprintln(stdout, "No differences found")
			return nil
		}
		return tw.Flush()

	default:
		db, err := openDBReadOnly(cfg.Paths.Root)
		if err != nil {
			return err
		}
		for _, name := range db.SortedNames() {
			fmt.Fprintf(stdout, "%s %s\n", styles.out.Bold(name), styles.out.Cyan(db.Packages[name].Version))
		}
		return nil
	}
}

// --- remove / uninstall ---

func cmdRemove(cfg config.Config, args []string, stdout, stderr io.Writer, rt RuntimeOptions, styles uiContext) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	targets := fs.Args()
	if len(targets) == 0 {
		return fmt.Errorf("usage: pe remove <package...>")
	}
	for _, name := range targets {
		if err := pkgops.RemovePackage(name, pkgops.RemoveOptions{
			Root:          cfg.Paths.Root,
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			ErrOut:        stderr,
		}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s\n", styles.out.BoldGreen("removed"), styles.out.Bold(name))
	}
	return nil
}

// --- helpers ---

func openDBReadOnly(root string) (*pkgdb.DB, error) {
	lock, err := pkgdb.AcquireLock(root, false)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return pkgdb.Open(root)
}

func loadLocalIndex(cfg config.Config) (repo.Index, error) {
	path := cfg.LocalIndexPath()
	idx, err := repo.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			return repo.Index{}, fmt.Errorf("local index %s not found; run `pe sync` first", path)
		}
		if errors.Is(err, os.ErrPermission) {
			return repo.Index{}, fmt.Errorf("cannot read local index %s: permission denied (run `sudo pe sync` to refresh permissions)", path)
		}
		return repo.Index{}, err
	}
	return idx, nil
}

func discoverPortMap(cfg config.Config) map[string]ports.Port {
	result := make(map[string]ports.Port)
	all, err := ports.DiscoverAll(cfg.Paths.PortsDir, cfg.Paths.Collections)
	if err != nil {
		return result
	}
	// Preserve collection priority order (first occurrence wins).
	colRank := make(map[string]int, len(cfg.Paths.Collections))
	for i, c := range cfg.Paths.Collections {
		colRank[c] = i
	}
	rankOf := func(col string) int {
		if r, ok := colRank[col]; ok {
			return r
		}
		return len(colRank) + 1
	}
	for _, p := range all {
		if existing, ok := result[p.Name]; !ok || rankOf(p.Collection) < rankOf(existing.Collection) {
			result[p.Name] = p
		}
	}
	return result
}

func resolveBinaryDeps(cfg config.Config, idx repo.Index, targets []string, installed map[string]pkgdb.PkgInfo) []string {
	portMap := discoverPortMap(cfg)
	targetSet := make(map[string]bool, len(targets))
	for _, t := range targets {
		targetSet[t] = true
	}

	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	var ordered []string

	var dfs func(name string)
	dfs = func(name string) {
		if visited[name] || visiting[name] {
			return
		}
		if !targetSet[name] {
			if _, ok := installed[name]; ok {
				visited[name] = true
				return
			}
		}
		visiting[name] = true
		var deps []string
		if entry, ok := repo.Find(idx, name, cfg.Repo.Arch); ok && len(entry.Depends) > 0 {
			deps = entry.Depends
		} else if p, ok := portMap[name]; ok {
			deps = p.Depends
		}
		for _, dep := range deps {
			dfs(dep)
		}
		delete(visiting, name)
		if !visited[name] {
			visited[name] = true
			ordered = append(ordered, name)
		}
	}

	for _, t := range targets {
		dfs(t)
	}
	return ordered
}

// parseInterspersedFlags allows subcommand flags to appear before or after positional arguments
// (e.g. `pe install zig --force` works identically to `pe install --force zig`), while preserving
// `--` as the end-of-flags delimiter.
func parseInterspersedFlags(fs *flag.FlagSet, args []string) error {
	type boolFlagger interface {
		IsBoolFlag() bool
	}
	var flags []string
	var positionals []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}

		// Flag token: -f, --force, --files=foo, or --files foo
		trimmed := strings.TrimLeft(arg, "-")
		name := trimmed
		hasEq := false
		if eq := strings.IndexByte(trimmed, '='); eq >= 0 {
			name = trimmed[:eq]
			hasEq = true
		}
		flags = append(flags, arg)
		if hasEq {
			continue
		}
		f := fs.Lookup(name)
		if f != nil {
			if bf, ok := f.Value.(boolFlagger); ok && bf.IsBoolFlag() {
				continue
			}
			// Value flag expects the next argument.
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}

	reordered := make([]string, 0, len(flags)+len(positionals)+1)
	reordered = append(reordered, flags...)
	if len(positionals) > 0 {
		reordered = append(reordered, "--")
		reordered = append(reordered, positionals...)
	}
	return fs.Parse(reordered)
}

// Keep filepath imported cleanly if needed in future expansions.
var _ = filepath.Join
