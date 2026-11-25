package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/check"
	"github.com/kryx-linux/kpm/internal/config"
	"github.com/kryx-linux/kpm/internal/depgraph"
	"github.com/kryx-linux/kpm/internal/pkgdb"
	"github.com/kryx-linux/kpm/internal/pkgops"
	"github.com/kryx-linux/kpm/internal/ports"
	"github.com/kryx-linux/kpm/internal/rejected"
	"github.com/kryx-linux/kpm/internal/repo"
	"github.com/kryx-linux/kpm/internal/state"
	"github.com/kryx-linux/kpm/internal/tsort"
)

// --- 1. kpm info <pkg> ---

func cmdInfo(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kpm info <package...>")
	}

	db, _ := openDBReadOnly(cfg.Paths.Root)
	world, _ := state.LoadWorld(cfg.Paths.Root)
	held, _ := state.LoadHold(cfg.Paths.Root, cfg.Repo.Hold)
	idx, _ := loadLocalIndex(cfg)
	portMap := discoverPortMap(cfg)
	revDeps := buildReverseDepMap(db, idx, portMap, cfg.Repo.Arch)

	for i, name := range fs.Args() {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		var (
			version     string
			collection  string
			description string
			urlStr      string
			deps        []string
			makeDeps    []string
			found       bool
		)
		instStatus := "no"
		if db != nil && db.FindPkg(name) {
			found = true
			instVer := db.Packages[name].Version
			reason := "dependency"
			if world[name] {
				reason = "explicit"
			}
			extra := ""
			if held[name] {
				extra = ", held"
			}
			instStatus = fmt.Sprintf("yes (%s, %s%s)", instVer, reason, extra)
			version = instVer
		}
		if entry, ok := repo.Find(idx, name, cfg.Repo.Arch); ok {
			found = true
			if version == "" || (db != nil && db.FindPkg(name) && entry.FullVersion() != version) {
				if version == "" {
					version = entry.FullVersion()
				} else {
					version = fmt.Sprintf("%s (available: %s)", version, entry.FullVersion())
				}
			}
			if collection == "" {
				collection = entry.Collection
			}
			if description == "" {
				description = entry.Description
			}
			if len(deps) == 0 {
				deps = entry.Depends
			}
			if len(makeDeps) == 0 {
				makeDeps = entry.MakeDepends
			}
		}
		if p, ok := portMap[name]; ok {
			found = true
			if version == "" {
				version = p.FullVersion()
			}
			if collection == "" {
				collection = p.Collection
			}
			if description == "" {
				description = p.Description
			}
			if urlStr == "" {
				urlStr = p.URL
			}
			if len(deps) == 0 {
				deps = p.Depends
			}
			if len(makeDeps) == 0 {
				makeDeps = p.MakeDepends
			}
		}

		if !found {
			return fmt.Errorf("package %q not found in installed db, binary index, or local ports", name)
		}
		if collection == "" {
			collection = "(none)"
		}
		if description == "" {
			description = "(none)"
		}
		if urlStr == "" {
			urlStr = "(none)"
		}

		reqBy := revDeps[name]
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Name:"), name)
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Version:"), version)
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Collection:"), collection)
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Description:"), description)
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("URL:"), urlStr)
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Installed:"), instStatus)
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Runtime Depends:"), formatListOrNone(deps))
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Make Depends:"), formatListOrNone(makeDeps))
		fmt.Fprintf(stdout, "%-16s %s\n", styles.out.Bold("Required By:"), formatListOrNone(reqBy))
	}
	return nil
}

// --- 2. kpm autoremove [--dry-run] ---

func cmdAutoremove(cfg config.Config, args []string, stdout, stderr io.Writer, rt RuntimeOptions, styles uiContext) error {
	fs := flag.NewFlagSet("autoremove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "show orphaned packages without removing them")
	dryRunShort := fs.Bool("d", false, "show orphaned packages without removing them")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	db, err := openDBReadOnly(cfg.Paths.Root)
	if err != nil {
		return err
	}
	world, err := state.LoadWorld(cfg.Paths.Root)
	if err != nil {
		return err
	}
	if len(world) == 0 {
		fmt.Fprintln(stdout, "no explicit packages recorded in world file; nothing to autoremove")
		return nil
	}

	idx, _ := loadLocalIndex(cfg)
	portMap := discoverPortMap(cfg)
	installedSet := make(map[string]bool, len(db.Packages))
	for name := range db.Packages {
		installedSet[name] = true
	}
	depsFn := func(pkg string) []string {
		return runtimeDepsOf(pkg, idx, portMap, cfg.Repo.Arch)
	}

	plan, err := depgraph.ResolveAutoremove(world, installedSet, depsFn)
	if err != nil {
		return err
	}
	orphans := plan.Order

	if len(orphans) == 0 {
		fmt.Fprintln(stdout, styles.out.Green("no orphaned packages found"))
		return nil
	}

	if *dryRun || *dryRunShort {
		fmt.Fprintf(stdout, "Orphaned package(s) to remove (%d):\n", len(orphans))
		for _, o := range orphans {
			fmt.Fprintf(stdout, "  %s %s\n", styles.out.Bold(o), styles.out.Cyan(db.Packages[o].Version))
		}
		return nil
	}

	// Remove strictly in reverse dependency order (dependents before their deps).
	for _, o := range orphans {
		if err := pkgops.RemovePackage(o, pkgops.RemoveOptions{
			Root:          cfg.Paths.Root,
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			ErrOut:        stderr,
		}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s orphan %s\n", styles.out.BoldGreen("removed"), styles.out.Bold(o))
	}
	return nil
}

// --- 3. kpm clean [--all] [--bin] [--sources] [--packages] [--keep N] [--dry-run] ---

func cmdClean(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cleanAll := fs.Bool("all", false, "clean bin, sources, and packages caches completely (sets --keep 0 unless specified)")
	cleanBin := fs.Bool("bin", false, "clean downloaded binary archive cache")
	cleanSources := fs.Bool("sources", false, "clean upstream source tarball cache")
	cleanPackages := fs.Bool("packages", false, "clean locally built package archive cache")
	keepN := fs.Int("keep", -1, "number of latest/installed versions to retain per package (default: 1, or 0 with --all)")
	dryRun := fs.Bool("dry-run", false, "show files that would be removed without deleting them")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	doBin := *cleanBin
	doSources := *cleanSources
	doPackages := *cleanPackages
	if *cleanAll {
		doBin, doSources, doPackages = true, true, true
	}
	if !doBin && !doSources && !doPackages {
		// Default: clean bin, packages, and partial downloads while keeping installed/latest versions.
		doBin = true
		doPackages = true
		doSources = true
	}

	keep := *keepN
	if keep < 0 {
		if *cleanAll {
			keep = 0
		} else {
			keep = 1
		}
	}

	installedVers := make(map[string]string)
	if db, err := openDBReadOnly(cfg.Paths.Root); err == nil {
		for name, info := range db.Packages {
			installedVers[name] = info.Version
		}
	}

	var removedCount int
	var reclaimedBytes int64
	removeFile := func(path string, size int64) error {
		if *dryRun {
			fmt.Fprintf(stdout, "would remove %s\n", path)
			removedCount++
			reclaimedBytes += size
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		removedCount++
		reclaimedBytes += size
		return nil
	}

	cleanArchiveDir := func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		type cachedArchive struct {
			path    string
			ver     string
			rel     string
			fullVer string
			size    int64
		}
		byPkg := make(map[string][]cachedArchive)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			full := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				continue
			}
			// Always purge partial / temporary downloads.
			if strings.HasSuffix(e.Name(), ".partial") || strings.HasPrefix(e.Name(), ".kpm-dl-") {
				if err := removeFile(full, info.Size()); err != nil {
					return err
				}
				continue
			}
			name, fullVer, _, ok := archive.ParsePkgFilename(e.Name())
			if !ok {
				continue
			}
			v, r, _ := archive.SplitVersionRelease(fullVer)
			byPkg[name] = append(byPkg[name], cachedArchive{
				path:    full,
				ver:     v,
				rel:     r,
				fullVer: fullVer,
				size:    info.Size(),
			})
		}

		for pkgName, list := range byPkg {
			sort.Slice(list, func(i, j int) bool {
				return repo.CompareVersion(list[i].ver, list[i].rel, list[j].ver, list[j].rel) > 0
			})
			instVer := installedVers[pkgName]
			kept := 0
			for _, item := range list {
				if keep > 0 {
					// Preserve currently installed version if present in cache, plus up to `keep` latest versions.
					if instVer != "" && item.fullVer == instVer {
						kept++
						continue
					}
					if kept < keep && (instVer != "" || *keepN > 0) {
						kept++
						continue
					}
				}
				if err := removeFile(item.path, item.size); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if doBin {
		if err := cleanArchiveDir(cfg.BinaryCacheDir()); err != nil {
			return err
		}
	}
	if doPackages {
		if err := cleanArchiveDir(cfg.Paths.PackageDir); err != nil {
			return err
		}
	}
	if doSources {
		activeSources := make(map[string]bool)
		if !*cleanAll && keep > 0 {
			for _, p := range discoverPortMap(cfg) {
				if _, inst := installedVers[p.Name]; inst {
					for i, s := range p.Source {
						if i < len(p.Renames) && p.Renames[i] != "" && p.Renames[i] != "SKIP" {
							activeSources[p.Renames[i]] = true
						} else {
							activeSources[filepath.Base(s)] = true
						}
					}
				}
			}
		}
		entries, err := os.ReadDir(cfg.Paths.SourceCache)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				full := filepath.Join(cfg.Paths.SourceCache, e.Name())
				info, ierr := e.Info()
				if ierr != nil {
					continue
				}
				if strings.HasSuffix(e.Name(), ".partial") || !activeSources[e.Name()] {
					if err := removeFile(full, info.Size()); err != nil {
						return err
					}
				}
			}
		}
	}

	action := "removed"
	if *dryRun {
		action = "would remove"
	}
	fmt.Fprintf(stdout, "%s %d cached file(s), freed %d bytes\n",
		styles.out.BoldGreen(action), removedCount, reclaimedBytes)
	return nil
}

// --- 4. kpm check [--broken] [--files] [<pkg...>] ---

func cmdCheck(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkBroken := fs.Bool("broken", false, "scan installed ELF binaries/libraries for missing .so dependencies (revdep)")
	checkFiles := fs.Bool("files", false, "verify all files recorded in /var/lib/pkg/db exist with expected types/permissions")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	// If neither --broken nor --files is passed, run general system health check (`kpm doctor`).
	if !*checkBroken && !*checkFiles {
		return cmdDoctor(cfg, fs.Args(), stdout, stderr, styles)
	}

	pkgs := fs.Args()
	hasFailure := false

	if *checkBroken {
		rep, err := check.CheckBroken(cfg.Paths.Root, pkgs)
		if err != nil {
			return err
		}
		if len(rep.BrokenFiles) == 0 {
			fmt.Fprintf(stdout, "%s scanned %d ELF file(s): no broken shared library dependencies found\n",
				styles.out.BoldGreen("[OK]"), rep.ScannedELF)
		} else {
			hasFailure = true
			for _, bf := range rep.BrokenFiles {
				fmt.Fprintf(stderr, "%s %s: /%s missing: %s\n",
					styles.err.BoldRed("BROKEN"),
					styles.err.Bold(bf.Package),
					bf.Path,
					strings.Join(bf.MissingLibs, ", "),
				)
			}
			fmt.Fprintf(stderr, "Affected package(s) (%d): %s\n",
				len(rep.AffectedPackages), strings.Join(rep.AffectedPackages, " "))
		}
	}

	if *checkFiles {
		rep, err := check.CheckFiles(cfg.Paths.Root, pkgs)
		if err != nil {
			return err
		}
		if len(rep.Issues) == 0 {
			fmt.Fprintf(stdout, "%s verified %d file(s) across %d package(s): all files intact\n",
				styles.out.BoldGreen("[OK]"), rep.CheckedFiles, rep.CheckedPackages)
		} else {
			hasFailure = true
			for _, iss := range rep.Issues {
				fmt.Fprintf(stderr, "%s %s: /%s (%s)\n",
					styles.err.BoldRed("MISSING/MISMATCH"),
					styles.err.Bold(iss.Package),
					iss.Path,
					iss.Reason,
				)
			}
			fmt.Fprintf(stderr, "File verification found %d issue(s) across %d package(s)\n",
				len(rep.Issues), rep.CheckedPackages)
		}
	}

	if hasFailure {
		return fmt.Errorf("system check detected broken dependencies or file integrity issues")
	}
	return nil
}

// --- 5. kpm hold / kpm unhold ---

func cmdHold(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("hold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listOnly := fs.Bool("list", false, "list held packages")
	listShort := fs.Bool("l", false, "list held packages")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	if *listOnly || *listShort || fs.NArg() == 0 {
		held, err := state.LoadHold(cfg.Paths.Root, cfg.Repo.Hold)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(held))
		for k := range held {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintln(stdout, n)
		}
		return nil
	}

	if err := state.AddHold(cfg.Paths.Root, fs.Args()...); err != nil {
		return err
	}
	for _, p := range fs.Args() {
		fmt.Fprintf(stdout, "%s %s\n", styles.out.BoldYellow("held"), styles.out.Bold(p))
	}
	return nil
}

func cmdUnhold(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("unhold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kpm unhold <package...>")
	}
	cfgHeld := make(map[string]bool, len(cfg.Repo.Hold))
	for _, p := range cfg.Repo.Hold {
		cfgHeld[p] = true
	}
	removed, err := state.RemoveHold(cfg.Paths.Root, fs.Args()...)
	if err != nil {
		return err
	}
	removedSet := make(map[string]bool, len(removed))
	for _, p := range removed {
		removedSet[p] = true
		fmt.Fprintf(stdout, "%s %s\n", styles.out.BoldGreen("unheld"), styles.out.Bold(p))
	}
	for _, p := range fs.Args() {
		if cfgHeld[p] {
			fmt.Fprintf(stderr, "%s %s is still pinned via [repo] hold in kpm.ini; remove it there to unhold permanently\n",
				styles.err.BoldYellow("warning:"), p)
		} else if !removedSet[p] {
			fmt.Fprintf(stderr, "%s %s was not held\n", styles.err.BoldYellow("note:"), p)
		}
	}
	return nil
}

// --- 6. kpm downgrade <pkg>[=<ver>] ---

func cmdDowngrade(cfg config.Config, args []string, stdout, stderr io.Writer, rt RuntimeOptions, styles uiContext) error {
	fs := flag.NewFlagSet("downgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "overwrite conflicting files")
	forceShort := fs.Bool("f", false, "overwrite conflicting files")
	verbose := fs.Bool("verbose", false, "list all conflicting files without truncation")
	verboseShort := fs.Bool("v", false, "list all conflicting files without truncation")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kpm downgrade [-f|--force] <package>[=<version>]...")
	}

	db, err := openDBReadOnly(cfg.Paths.Root)
	if err != nil {
		return err
	}
	idx, _ := loadLocalIndex(cfg)
	timeout := time.Duration(cfg.Repo.TimeoutSeconds) * time.Second
	doForce := *force || *forceShort
	doVerbose := *verbose || *verboseShort

	for _, rawTarget := range fs.Args() {
		name, wantVer := parsePkgVersionSpec(rawTarget)
		if !db.FindPkg(name) {
			return fmt.Errorf("package %q is not installed", name)
		}
		curVer := db.Packages[name].Version

		var localPath string
		var targetVer string

		if wantVer != "" {
			if cached, ok := findCachedBinaryVersion(cfg.BinaryCacheDir(), name, wantVer); ok {
				localPath = cached
				targetVer = wantVer
			} else if entry, ok := repo.FindVersion(idx, name, wantVer, cfg.Repo.Arch); ok {
				p, ferr := repo.FetchBinaryWithStyle(entry, cfg.BinaryCacheDir(), timeout, stdout, styles.out)
				if ferr != nil {
					return ferr
				}
				localPath = p
				targetVer = entry.FullVersion()
			} else {
				return fmt.Errorf("version %s of %s not found in binary cache or index.json", wantVer, name)
			}
		} else {
			// Pick highest version strictly older than curVer from local binary cache and index.json.
			bestCachePath, bestCacheVer, hasCache := findPreviousCachedBinary(cfg.BinaryCacheDir(), name, curVer)
			idxEntry, hasIdx := repo.FindPreviousVersion(idx, name, curVer, cfg.Repo.Arch)

			switch {
			case !hasCache && !hasIdx:
				return fmt.Errorf("no older version available for %s (current: %s) in cache or index.json", name, curVer)
			case hasCache && !hasIdx:
				localPath = bestCachePath
				targetVer = bestCacheVer
			case !hasCache && hasIdx:
				p, ferr := repo.FetchBinaryWithStyle(idxEntry, cfg.BinaryCacheDir(), timeout, stdout, styles.out)
				if ferr != nil {
					return ferr
				}
				localPath = p
				targetVer = idxEntry.FullVersion()
			default:
				cv, cr, _ := archive.SplitVersionRelease(bestCacheVer)
				if repo.CompareVersion(cv, cr, idxEntry.Pkgver, idxEntry.Pkgrel) >= 0 {
					localPath = bestCachePath
					targetVer = bestCacheVer
				} else {
					p, ferr := repo.FetchBinaryWithStyle(idxEntry, cfg.BinaryCacheDir(), timeout, stdout, styles.out)
					if ferr != nil {
						return ferr
					}
					localPath = p
					targetVer = idxEntry.FullVersion()
				}
			}
		}

		pkgName, newVer, ierr := pkgops.InstallArchive(localPath, pkgops.InstallOptions{
			Root:          cfg.Paths.Root,
			PkgaddConf:    cfg.Paths.PkgaddConf,
			Upgrade:       true,
			Force:         doForce,
			Verbose:       doVerbose,
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			Out:           stdout,
			ErrOut:        stderr,
		})
		if ierr != nil {
			return ierr
		}
		fmt.Fprintf(stdout, "%s %s %s -> %s\n",
			styles.out.BoldYellow("downgraded"),
			styles.out.Bold(pkgName),
			styles.out.Cyan(curVer),
			styles.out.Green(newVer),
		)
		_ = targetVer
	}
	return nil
}

// --- 7. kpm rejected [--list|--diff|--keep|--upgrade|--merge] ---

func cmdRejected(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("rejected", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listFlag := fs.Bool("list", false, "list pending rejected files")
	listShort := fs.Bool("l", false, "list pending rejected files")
	diffFlag := fs.Bool("diff", false, "show unified diff for rejected file(s)")
	keepTarget := fs.String("keep", "", "keep installed version and delete rejected copy (<file|all>)")
	upgradeTarget := fs.String("upgrade", "", "replace installed file with rejected version (<file|all>)")
	mergeTarget := fs.String("merge", "", "merge rejected file into installed file with conflict markers (<file|all>)")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	entries, err := rejected.List(cfg.Paths.Root)
	if err != nil {
		return err
	}

	switch {
	case *listFlag || *listShort:
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "no rejected files")
			return nil
		}
		for _, e := range entries {
			fmt.Fprintf(stdout, "/%s\n", e.RelPath)
		}
		return nil

	case *diffFlag:
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "no rejected files")
			return nil
		}
		selected := entries
		if fs.NArg() > 0 {
			selected, err = rejected.ResolveTarget(entries, fs.Arg(0))
			if err != nil {
				return err
			}
		}
		for _, e := range selected {
			d, derr := rejected.Diff(e)
			if derr != nil {
				return derr
			}
			fmt.Fprint(stdout, d)
		}
		return nil

	case strings.TrimSpace(*keepTarget) != "":
		selected, err := rejected.ResolveTarget(entries, *keepTarget)
		if err != nil {
			return err
		}
		for _, e := range selected {
			if err := rejected.Keep(cfg.Paths.Root, e); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s /%s (removed rejected copy)\n", styles.out.BoldGreen("kept"), e.RelPath)
		}
		return nil

	case strings.TrimSpace(*upgradeTarget) != "":
		selected, err := rejected.ResolveTarget(entries, *upgradeTarget)
		if err != nil {
			return err
		}
		for _, e := range selected {
			if err := rejected.Upgrade(cfg.Paths.Root, e); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s /%s\n", styles.out.BoldGreen("upgraded"), e.RelPath)
		}
		return nil

	case strings.TrimSpace(*mergeTarget) != "":
		selected, err := rejected.ResolveTarget(entries, *mergeTarget)
		if err != nil {
			return err
		}
		for _, e := range selected {
			hadConflicts, merr := rejected.MergeFile(cfg.Paths.Root, e)
			if merr != nil {
				return merr
			}
			if hadConflicts {
				fmt.Fprintf(stdout, "%s /%s (conflict markers written — review before reboot)\n",
					styles.out.BoldYellow("merged"), e.RelPath)
			} else {
				fmt.Fprintf(stdout, "%s /%s\n", styles.out.BoldGreen("merged"), e.RelPath)
			}
		}
		return nil

	default:
		return rejected.RunInteractive(rejected.Options{
			Root:        cfg.Paths.Root,
			ProgName:    "kpm",
			In:          os.Stdin,
			Out:         stdout,
			ErrOut:      stderr,
			Interactive: true,
		})
	}
}

// --- shared helpers for Part 4 ---

func parsePkgVersionSpec(spec string) (name, version string) {
	if eq := strings.IndexByte(spec, '='); eq > 0 && eq < len(spec)-1 {
		return spec[:eq], spec[eq+1:]
	}
	return spec, ""
}

func runtimeDepsOf(pkg string, idx repo.Index, portMap map[string]ports.Port, arch string) []string {
	if e, ok := repo.Find(idx, pkg, arch); ok && len(e.Depends) > 0 {
		return e.Depends
	}
	if p, ok := portMap[pkg]; ok && len(p.Depends) > 0 {
		return p.Depends
	}
	return nil
}

func buildReverseDepMap(db *pkgdb.DB, idx repo.Index, portMap map[string]ports.Port, arch string) map[string][]string {
	if db == nil {
		return make(map[string][]string)
	}
	installedSet := make(map[string]bool, len(db.Packages))
	for name := range db.Packages {
		installedSet[name] = true
	}
	return depgraph.ReverseDeps(installedSet, func(pkg string) []string {
		return runtimeDepsOf(pkg, idx, portMap, arch)
	})
}

// --- 8. kpm deps / kpm graph ---

func cmdDeps(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("deps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showTree := fs.Bool("tree", false, "display dependency tree")
	showTreeShort := fs.Bool("t", false, "display dependency tree")
	reverse := fs.Bool("reverse", false, "show reverse dependencies (dependents)")
	reverseShort := fs.Bool("R", false, "show reverse dependencies (dependents)")
	showWaves := fs.Bool("waves", false, "group dependencies into parallel topological waves")
	showWavesShort := fs.Bool("w", false, "group dependencies into parallel topological waves")
	showDOT := fs.Bool("dot", false, "export Graphviz DOT digraph")
	showCycles := fs.Bool("cycles", false, "check and display any dependency cycles (SCCs)")
	withMake := fs.Bool("make", false, "include build-only MakeDepends in addition to runtime Depends")
	maxDepth := fs.Int("depth", 0, "maximum depth for --tree (0 = unlimited)")
	jsonOut := fs.Bool("json", false, "emit dependency graph report as JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}

	targets := fs.Args()
	doTree := *showTree || *showTreeShort
	doReverse := *reverse || *reverseShort
	doWaves := *showWaves || *showWavesShort

	db, _ := openDBReadOnly(cfg.Paths.Root)
	idx, _ := loadLocalIndex(cfg)
	portMap := discoverPortMap(cfg)

	depsFn := func(pkg string) []string {
		var rdeps, mdeps []string
		if e, ok := repo.Find(idx, pkg, cfg.Repo.Arch); ok {
			rdeps = e.Depends
			mdeps = e.MakeDepends
		}
		if p, ok := portMap[pkg]; ok {
			if len(rdeps) == 0 {
				rdeps = p.Depends
			}
			if len(mdeps) == 0 {
				mdeps = p.MakeDepends
			}
		}
		if !*withMake || len(mdeps) == 0 {
			return rdeps
		}
		out := make([]string, 0, len(rdeps)+len(mdeps))
		seen := make(map[string]bool, len(rdeps)+len(mdeps))
		for _, d := range rdeps {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
		for _, d := range mdeps {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
		return out
	}

	// Build known package universe for reverse-dep or full-graph cycle audits.
	universeSet := make(map[string]bool)
	if db != nil {
		for name := range db.Packages {
			universeSet[name] = true
		}
	}
	for name := range idx.Packages {
		universeSet[name] = true
	}
	for name := range portMap {
		universeSet[name] = true
	}
	universe := make([]string, 0, len(universeSet))
	for name := range universeSet {
		universe = append(universe, name)
	}
	sort.Strings(universe)

	if len(targets) == 0 {
		if *showCycles {
			targets = universe
		} else {
			return fmt.Errorf("usage: kpm deps [--tree] [--reverse] [--waves] [--dot] [--cycles] [--make] [--depth N] [--json] <package...>")
		}
	}

	g := depgraph.BuildSubgraph(targets, depsFn, doReverse, universe)
	dag := tsort.Condense[string](g)
	order, _, _ := tsort.SortCondensed[string](g, depgraph.ComparePkgPriority, false)
	waves, _ := tsort.Waves[string](g, depgraph.ComparePkgPriority)

	cycles := make([]depgraph.CycleInfo, len(dag.Cycles))
	for i := range dag.Cycles {
		members := make([]string, len(dag.Cycles[i]))
		copy(members, dag.Cycles[i])
		sort.Strings(members)
		cycles[i] = depgraph.CycleInfo{
			Members: members,
			Path:    dag.CyclePaths[i],
		}
	}

	if *jsonOut {
		report := struct {
			Targets []string             `json:"targets"`
			Reverse bool                 `json:"reverse"`
			Order   []string             `json:"order"`
			Waves   [][]string           `json:"waves"`
			Cycles  []depgraph.CycleInfo `json:"cycles,omitempty"`
		}{
			Targets: targets,
			Reverse: doReverse,
			Order:   order,
			Waves:   waves,
			Cycles:  cycles,
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	if *showDOT {
		title := "kpm_deps"
		if len(targets) == 1 {
			title = targets[0]
		}
		fmt.Fprint(stdout, depgraph.RenderDOT(g, title))
		return nil
	}

	if *showCycles {
		if len(cycles) == 0 {
			fmt.Fprintf(stdout, "%s no dependency cycles found across %d package(s)\n",
				styles.out.BoldGreen("[OK]"), len(order))
			return nil
		}
		for _, c := range cycles {
			fmt.Fprintf(stderr, "%s %s\n", styles.err.BoldRed("CYCLE"), c.String())
		}
		return fmt.Errorf("detected %d dependency cycle(s)", len(cycles))
	}

	if doTree {
		treeFn := depsFn
		if doReverse {
			revMap := depgraph.ReverseDeps(universeSet, depsFn)
			treeFn = func(pkg string) []string { return revMap[pkg] }
		}
		for i, t := range targets {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprint(stdout, depgraph.RenderTree(t, treeFn, *maxDepth))
		}
		return nil
	}

	if doWaves {
		for w, layer := range waves {
			fmt.Fprintf(stdout, "%s %s\n",
				styles.out.BoldCyan(fmt.Sprintf("Wave %d:", w)),
				strings.Join(layer, " "),
			)
		}
		return nil
	}

	for _, c := range cycles {
		fmt.Fprintf(stderr, "%s dependency cycle detected: %s\n", styles.err.BoldYellow("warning:"), c.String())
	}
	for _, pkg := range order {
		fmt.Fprintln(stdout, pkg)
	}
	return nil
}

// --- 9. kpm why <pkg...> ---

func cmdWhy(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	fs := flag.NewFlagSet("why", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "output explanation as JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kpm why [--json] <package...>")
	}

	db, err := openDBReadOnly(cfg.Paths.Root)
	if err != nil {
		return err
	}
	world, _ := state.LoadWorld(cfg.Paths.Root)
	idx, _ := loadLocalIndex(cfg)
	portMap := discoverPortMap(cfg)

	installedSet := make(map[string]bool, len(db.Packages))
	for name := range db.Packages {
		installedSet[name] = true
	}
	depsFn := func(pkg string) []string {
		return runtimeDepsOf(pkg, idx, portMap, cfg.Repo.Arch)
	}

	results := make([]depgraph.WhyResult, 0, fs.NArg())
	for _, target := range fs.Args() {
		results = append(results, depgraph.Why(target, world, installedSet, depsFn))
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(results)
	}

	for i, res := range results {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		if !res.Installed {
			fmt.Fprintf(stdout, "%s is %s\n", styles.out.Bold(res.Target), styles.out.Yellow("not installed"))
			continue
		}
		if res.IsExplicit {
			fmt.Fprintf(stdout, "%s is %s (in /var/lib/kpm/world)\n",
				styles.out.Bold(res.Target), styles.out.BoldGreen("explicitly installed"))
		}
		if len(res.Paths) > 0 {
			fmt.Fprintf(stdout, "%s is required by:\n", styles.out.Bold(res.Target))
			for _, p := range res.Paths {
				fmt.Fprintf(stdout, "  %s\n", strings.Join(p, " -> "))
			}
		} else if res.IsOrphan {
			fmt.Fprintf(stdout, "%s is an %s (not in world and no world package depends on it; run 'kpm autoremove')\n",
				styles.out.Bold(res.Target), styles.out.BoldYellow("orphan"))
		}
	}
	return nil
}

func formatListOrNone(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}

func findCachedBinaryVersion(cacheDir, pkgName, wantVer string) (string, bool) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, fullVer, _, ok := archive.ParsePkgFilename(e.Name())
		if !ok || name != pkgName {
			continue
		}
		v, _, _ := archive.SplitVersionRelease(fullVer)
		if fullVer == wantVer || v == wantVer {
			return filepath.Join(cacheDir, e.Name()), true
		}
	}
	return "", false
}

func findPreviousCachedBinary(cacheDir, pkgName, curFullVer string) (bestPath, bestVer string, ok bool) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return "", "", false
	}
	curV, curR, _ := archive.SplitVersionRelease(curFullVer)
	var bestV, bestR string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, fullVer, _, parsed := archive.ParsePkgFilename(e.Name())
		if !parsed || name != pkgName || fullVer == curFullVer {
			continue
		}
		v, r, _ := archive.SplitVersionRelease(fullVer)
		if repo.CompareVersion(v, r, curV, curR) >= 0 {
			continue
		}
		if !ok || repo.CompareVersion(v, r, bestV, bestR) > 0 {
			ok = true
			bestV, bestR = v, r
			bestVer = fullVer
			bestPath = filepath.Join(cacheDir, e.Name())
		}
	}
	return bestPath, bestVer, ok
}
