package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/builder"
	"github.com/kryx-linux/kpm/internal/config"
	"github.com/kryx-linux/kpm/internal/pkgops"
	"github.com/kryx-linux/kpm/internal/ports"
	"github.com/kryx-linux/kpm/internal/portscli"
	"github.com/kryx-linux/kpm/internal/rejected"
	"github.com/kryx-linux/kpm/internal/state"
)

// RunWithArgv0 dispatches based on filepath.Base(argv0), enabling 100% upstream
// CRUX multi-call symlink compatibility for pkgadd, pkgrm, pkginfo, pkgmk,
// ports, and rejmerge alongside native kpm invocation.
func RunWithArgv0(argv0 string, args []string, stdout, stderr io.Writer) int {
	return RunWithArgv0Options(argv0, args, stdout, stderr, RuntimeOptions{})
}

// RunWithArgv0Options is like RunWithArgv0 with injectable RuntimeOptions for tests.
func RunWithArgv0Options(argv0 string, args []string, stdout, stderr io.Writer, rt RuntimeOptions) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	base := filepath.Base(argv0)
	switch base {
	case "pkgadd":
		return runPkgadd(args, stdout, stderr, rt)
	case "pkgrm":
		return runPkgrm(args, stdout, stderr, rt)
	case "pkginfo":
		return runPkginfo(args, stdout, stderr)
	case "pkgmk":
		return runPkgmk(args, stdout, stderr, rt)
	case "ports", "gports":
		return runPorts(args, stdout, stderr)
	case "rejmerge":
		return runRejmerge(args, stdout, stderr)
	case "prt-get":
		return runPrtGet(args, stdout, stderr)
	default:
		return RunWithOptions(args, stdout, stderr, rt)
	}
}

// --- pkgadd (100% parity with upstream CRUX pkgutils/pkgadd.cc) ---

func runPkgadd(args []string, stdout, stderr io.Writer, rt RuntimeOptions) int {
	root := "/"
	confFile := "/etc/pkgadd.conf"
	upgrade := false
	force := false
	var pkgFile string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, `usage: pkgadd [options] <file>
options:
  -u, --upgrade       upgrade package with the same name
  -f, --force         force install, overwrite conflicting files
  -r, --root <path>   specify alternative installation root
  -c, --config <file> use alternate configuration file
  -v, --version       print version and exit
  -h, --help          print help and exit
`)
			return 0
		case a == "-v" || a == "--version":
			fmt.Fprintf(stdout, "pkgadd (kpm) %s\n", Version)
			return 0
		case a == "-u" || a == "--upgrade":
			upgrade = true
		case a == "-f" || a == "--force":
			force = true
		case a == "-uf" || a == "-fu":
			upgrade = true
			force = true
		case a == "-r" || a == "--root":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkgadd: option %s requires an argument\n", a)
				return 1
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkgadd: option %s requires an argument\n", a)
				return 1
			}
			confFile = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			confFile = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(stderr, "pkgadd: invalid option %s\n", a)
			return 1
		default:
			if pkgFile != "" {
				fmt.Fprintf(stderr, "pkgadd: only one package file may be specified\n")
				return 1
			}
			pkgFile = a
		}
	}

	if pkgFile == "" {
		fmt.Fprintf(stderr, "pkgadd: argument missing\n")
		return 1
	}

	name, _, err := pkgops.InstallArchive(pkgFile, pkgops.InstallOptions{
		Root:          root,
		PkgaddConf:    confFile,
		Upgrade:       upgrade,
		Force:         force,
		Verbose:       true, // upstream pkgadd lists all conflicting files
		LogPrefix:     "pkgadd",
		SkipRootCheck: rt.SkipRootCheck,
		SkipLdconfig:  rt.SkipLdconfig,
		Out:           stdout,
		ErrOut:        stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "pkgadd: %v\n", err)
		return 1
	}
	if !upgrade {
		_ = state.AddToWorld(root, name)
	}
	return 0
}

// --- pkgrm (100% parity with upstream CRUX pkgutils/pkgrm.cc) ---

func runPkgrm(args []string, stdout, stderr io.Writer, rt RuntimeOptions) int {
	root := "/"
	var pkgName string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, `usage: pkgrm [options] <package>
options:
  -r, --root <path>   specify alternative installation root
  -v, --version       print version and exit
  -h, --help          print help and exit
`)
			return 0
		case a == "-v" || a == "--version":
			fmt.Fprintf(stdout, "pkgrm (kpm) %s\n", Version)
			return 0
		case a == "-r" || a == "--root":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkgrm: option %s requires an argument\n", a)
				return 1
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(stderr, "pkgrm: invalid option %s\n", a)
			return 1
		default:
			if pkgName != "" {
				fmt.Fprintf(stderr, "pkgrm: only one package may be specified\n")
				return 1
			}
			pkgName = a
		}
	}

	if pkgName == "" {
		fmt.Fprintf(stderr, "pkgrm: argument missing\n")
		return 1
	}

	if err := pkgops.RemovePackage(pkgName, pkgops.RemoveOptions{
		Root:          root,
		SkipRootCheck: rt.SkipRootCheck,
		SkipLdconfig:  rt.SkipLdconfig,
		ErrOut:        stderr,
	}); err != nil {
		fmt.Fprintf(stderr, "pkgrm: %v\n", err)
		return 1
	}
	_ = state.RemoveFromWorld(root, pkgName)
	return 0
}

// --- pkginfo (100% parity with upstream CRUX pkgutils/pkginfo.cc) ---

func runPkginfo(args []string, stdout, stderr io.Writer) int {
	root := "/"
	mode := ""
	arg := ""
	modeCount := 0

	setMode := func(m, a string) {
		mode = m
		arg = a
		modeCount++
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, `usage: pkginfo [options]
options:
  -i, --installed             list installed packages
  -l, --list <package|file>   list files in <package> or <file>
  -o, --owner <pattern>       list owner(s) of file(s) matching <pattern>
  -f, --footprint <file>      print footprint for <file>
  -r, --root <path>           specify alternative installation root
  -v, --version               print version and exit
  -h, --help                  print help and exit
`)
			return 0
		case a == "-v" || a == "--version":
			fmt.Fprintf(stdout, "pkginfo (kpm) %s\n", Version)
			return 0
		case a == "-i" || a == "--installed":
			setMode("installed", "")
		case a == "-l" || a == "--list":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkginfo: option %s requires an argument\n", a)
				return 1
			}
			setMode("list", args[i+1])
			i++
		case strings.HasPrefix(a, "--list="):
			setMode("list", strings.TrimPrefix(a, "--list="))
		case a == "-o" || a == "--owner":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkginfo: option %s requires an argument\n", a)
				return 1
			}
			setMode("owner", args[i+1])
			i++
		case strings.HasPrefix(a, "--owner="):
			setMode("owner", strings.TrimPrefix(a, "--owner="))
		case a == "-f" || a == "--footprint":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkginfo: option %s requires an argument\n", a)
				return 1
			}
			setMode("footprint", args[i+1])
			i++
		case strings.HasPrefix(a, "--footprint="):
			setMode("footprint", strings.TrimPrefix(a, "--footprint="))
		case a == "-r" || a == "--root":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "pkginfo: option %s requires an argument\n", a)
				return 1
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		default:
			fmt.Fprintf(stderr, "pkginfo: invalid option %s\n", a)
			return 1
		}
	}

	if modeCount == 0 {
		fmt.Fprintf(stderr, "pkginfo: option missing\n")
		return 1
	}
	if modeCount > 1 {
		fmt.Fprintf(stderr, "pkginfo: too many options\n")
		return 1
	}

	switch mode {
	case "installed":
		db, err := openDBReadOnly(root)
		if err != nil {
			fmt.Fprintf(stderr, "pkginfo: %v\n", err)
			return 1
		}
		for _, name := range db.SortedNames() {
			fmt.Fprintf(stdout, "%s %s\n", name, db.Packages[name].Version)
		}
		return 0

	case "list":
		db, err := openDBReadOnly(root)
		if err == nil && db.FindPkg(arg) {
			for _, f := range db.Packages[arg].SortedFiles() {
				fmt.Fprintln(stdout, f)
			}
			return 0
		}
		if fi, serr := os.Stat(arg); serr == nil && !fi.IsDir() {
			_, info, oerr := archive.Open(arg)
			if oerr != nil {
				fmt.Fprintf(stderr, "pkginfo: %v\n", oerr)
				return 1
			}
			for _, f := range info.SortedFiles() {
				fmt.Fprintln(stdout, f)
			}
			return 0
		}
		fmt.Fprintf(stderr, "pkginfo: %s is neither an installed package nor a package file\n", arg)
		return 1

	case "owner":
		re, err := regexp.Compile(arg)
		if err != nil {
			fmt.Fprintf(stderr, "pkginfo: error compiling regular expression: %v\n", err)
			return 1
		}
		db, err := openDBReadOnly(root)
		if err != nil {
			fmt.Fprintf(stderr, "pkginfo: %v\n", err)
			return 1
		}
		type pair struct{ pkg, file string }
		var matches []pair
		maxWidth := len("Package")
		for _, name := range db.SortedNames() {
			for _, f := range db.Packages[name].SortedFiles() {
				if re.MatchString("/" + f) {
					matches = append(matches, pair{name, f})
					if len(name) > maxWidth {
						maxWidth = len(name)
					}
				}
			}
		}
		if len(matches) == 0 {
			fmt.Fprintln(stdout, "pkginfo: no owner(s) found")
			return 0
		}
		// Exact CRUX pkginfo.cc width alignment: width = max(len(pkg)) + 2 spaces
		fmt.Fprintf(stdout, "%-*s%s\n", maxWidth+2, "Package", "File")
		for _, m := range matches {
			fmt.Fprintf(stdout, "%-*s%s\n", maxWidth+2, m.pkg, m.file)
		}
		return 0

	case "footprint":
		fi, err := os.Stat(arg)
		if err != nil {
			fmt.Fprintf(stderr, "pkginfo: could not open %s: %v\n", arg, err)
			return 1
		}
		if fi.IsDir() {
			if err := archive.FootprintDir(arg, stdout); err != nil {
				fmt.Fprintf(stderr, "pkginfo: %v\n", err)
				return 1
			}
			return 0
		}
		if err := archive.Footprint(arg, stdout); err != nil {
			fmt.Fprintf(stderr, "pkginfo: %v\n", err)
			return 1
		}
		return 0
	}
	return 1
}

// --- pkgmk (100% parity with upstream CRUX pkgutils/pkgmk.in) ---

func runPkgmk(args []string, stdout, stderr io.Writer, rt RuntimeOptions) int {
	var (
		doInstall     bool
		doUpgrade     bool
		recursive     bool
		download      bool
		downloadOnly  bool
		extractOnly   bool
		upToDate      bool
		updateFP      bool
		ignoreFP      bool
		ignoreNew     bool
		updateMD5     bool
		ignoreMD5     bool
		checkMD5      bool
		updateSig     bool
		checkSig      bool
		ignoreSig     bool
		refreshSig    bool
		secretKey     string
		publicKey     string
		noStrip       bool
		force         bool
		clean         bool
		keepWork      bool
		confFile      string
		explicitConf  bool
	)

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-h", "--help":
			fmt.Fprint(stdout, `usage: pkgmk [options]
options:
  -i,   --install             build and install package
  -u,   --upgrade             build and install package (as upgrade)
  -r,   --recursive           search for and build packages recursively
  -d,   --download            download missing source file(s)
  -do,  --download-only       do not build, only download missing source file(s)
  -eo,  --extract-only        do not build, only extract source file(s)
  -utd, --up-to-date          do not build, only check if package is up to date
  -uf,  --update-footprint    update footprint using result from last build
  -if,  --ignore-footprint    build package without checking footprint
  -in,  --ignore-new          build package, ignore new files in a footprint mismatch
  -um,  --update-md5sum       update md5sum
  -im,  --ignore-md5sum       build package without checking md5sum
  -cm,  --check-md5sum        do not build, only check md5sum
  -us,  --update-signature    update signature including sha256 checksums
  -cs,  --check-signature     do not build, only check signature
  -is,  --ignore-signature    build package without checking signature
  -rs,  --refresh-signature   create new signature and keep existing sha256 checksums
  -sk,  --secret-key <file>   specify secret key file for signing
  -pk,  --public-key <file>   specify public key file for signature verification
  -ns,  --no-strip            do not strip executable binaries or libraries
  -f,   --force               build package even if it appears to be up to date
  -c,   --clean               remove package and downloaded files
  -kw,  --keep-work           keep temporary working directory
  -cf,  --config-file <file>  use alternative configuration file
  -v,   --version             print version and exit
  -h,   --help                print help and exit
`)
			return 0
		case "-v", "--version":
			fmt.Fprintf(stdout, "pkgmk (kpm) %s\n", Version)
			return 0
		case "-i", "--install":
			doInstall = true
		case "-u", "--upgrade":
			doUpgrade = true
		case "-r", "--recursive":
			recursive = true
		case "-d", "--download":
			download = true
		case "-do", "--download-only":
			downloadOnly = true
			download = true
		case "-eo", "--extract-only":
			extractOnly = true
		case "-utd", "--up-to-date":
			upToDate = true
		case "-uf", "--update-footprint":
			updateFP = true
		case "-if", "--ignore-footprint":
			ignoreFP = true
		case "-in", "--ignore-new":
			ignoreNew = true
		case "-um", "--update-md5sum":
			updateMD5 = true
		case "-im", "--ignore-md5sum":
			ignoreMD5 = true
		case "-cm", "--check-md5sum":
			checkMD5 = true
		case "-us", "--update-signature":
			updateSig = true
		case "-cs", "--check-signature":
			checkSig = true
		case "-is", "--ignore-signature":
			ignoreSig = true
		case "-rs", "--refresh-signature":
			refreshSig = true
		case "-sk", "--secret-key":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "=======> ERROR: '%s' requires an argument\n", a)
				return builder.ExitGeneral
			}
			secretKey = args[i+1]
			i++
		case "-pk", "--public-key":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "=======> ERROR: '%s' requires an argument\n", a)
				return builder.ExitGeneral
			}
			publicKey = args[i+1]
			i++
		case "-ns", "--no-strip":
			noStrip = true
		case "-f", "--force":
			force = true
		case "-c", "--clean":
			clean = true
		case "-kw", "--keep-work":
			keepWork = true
		case "-cf", "--config-file":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "=======> ERROR: '%s' requires an argument\n", a)
				return builder.ExitGeneral
			}
			confFile = args[i+1]
			explicitConf = true
			i++
		default:
			fmt.Fprintf(stderr, "=======> ERROR: invalid option '%s'\n", a)
			return builder.ExitGeneral
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
		return builder.ExitGeneral
	}

	// Build config layered as: kpm.ini defaults -> pkgmk.conf (or -cf) -> PKGMK_* env vars.
	// Note: stock CRUX pkgmk defaults to storing built packages/sources in port's own directory
	// unless overridden by pkgmk.conf or PKGMK_* env.
	cfgPath := config.ResolvePath("")
	cfg, _ := config.Load(cfgPath, false)
	// When no explicit kpm.ini exists, default pkgmk directories to CWD (matching upstream pkgmk.in).
	if _, statErr := os.Stat(cfgPath); os.IsNotExist(statErr) && strings.TrimSpace(os.Getenv("KPM_CONFIG")) == "" {
		cfg.Paths.SourceCache = cwd
		cfg.Paths.PackageDir = cwd
		cfg.Paths.WorkDir = filepath.Join(cwd, "work")
		cfg.Build.DownloadSources = false
	}

	if explicitConf {
		if err := builder.LoadPkgmkConf(confFile, &cfg); err != nil {
			// Also allow passing an INI kpm.ini file to -cf.
			if loaded, lerr := config.Load(confFile, true); lerr == nil {
				cfg = loaded
			} else {
				fmt.Fprintf(stderr, "=======> ERROR: unable to load config '%s': %v\n", confFile, err)
				return builder.ExitGeneral
			}
		}
	} else if _, err := os.Stat("/etc/pkgmk.conf"); err == nil {
		_ = builder.LoadPkgmkConf("/etc/pkgmk.conf", &cfg)
	}
	builder.ApplyPkgmkEnv(&cfg)

	if download {
		cfg.Build.DownloadSources = true
	}

	var portDirs []string
	if recursive {
		dirs, derr := builder.DiscoverRecursivePorts(cwd)
		if derr != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", derr)
			return builder.ExitGeneral
		}
		portDirs = dirs
	} else {
		if _, statErr := os.Stat(filepath.Join(cwd, "Pkgfile")); statErr != nil {
			fmt.Fprintf(stderr, "=======> ERROR: File 'Pkgfile' not found.\n")
			return builder.ExitPkgfile
		}
		portDirs = []string{cwd}
	}

	lastCode := 0
	for _, pdir := range portDirs {
		portCfg := cfg
		if !explicitConf && portCfg.Paths.PackageDir == cwd && pdir != cwd {
			portCfg.Paths.PackageDir = pdir
		}
		if !explicitConf && portCfg.Paths.SourceCache == cwd && pdir != cwd {
			portCfg.Paths.SourceCache = pdir
		}
		if !explicitConf && portCfg.Paths.WorkDir == filepath.Join(cwd, "work") && pdir != cwd {
			portCfg.Paths.WorkDir = filepath.Join(pdir, "work")
		}
		port := ports.Port{Dir: pdir}

		code := runSinglePkgmk(portCfg, port, pkgmkFlags{
			doInstall:    doInstall,
			doUpgrade:    doUpgrade,
			downloadOnly: downloadOnly,
			extractOnly:  extractOnly,
			upToDate:     upToDate,
			updateFP:     updateFP,
			ignoreFP:     ignoreFP,
			ignoreNew:    ignoreNew,
			updateMD5:    updateMD5,
			ignoreMD5:    ignoreMD5,
			checkMD5:     checkMD5,
			updateSig:    updateSig,
			checkSig:     checkSig,
			ignoreSig:    ignoreSig,
			refreshSig:   refreshSig,
			secretKey:    secretKey,
			publicKey:    publicKey,
			noStrip:      noStrip,
			force:        force,
			clean:        clean,
			keepWork:     keepWork,
		}, stdout, stderr, rt)
		if code != 0 {
			if !recursive {
				return code
			}
			lastCode = code
		}
	}
	return lastCode
}

type pkgmkFlags struct {
	doInstall, doUpgrade             bool
	downloadOnly, extractOnly        bool
	upToDate, updateFP               bool
	ignoreFP, ignoreNew              bool
	updateMD5, ignoreMD5, checkMD5   bool
	updateSig, checkSig, ignoreSig   bool
	refreshSig                       bool
	secretKey, publicKey             string
	noStrip, force, clean, keepWork  bool
}

func runSinglePkgmk(cfg config.Config, port ports.Port, f pkgmkFlags, stdout, stderr io.Writer, rt RuntimeOptions) int {
	if f.clean {
		if err := builder.CleanPort(cfg, port, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.updateMD5 {
		if err := builder.UpdateMD5Sum(cfg, port, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.checkMD5 {
		if err := builder.CheckMD5Sum(cfg, port, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.updateSig {
		if err := builder.UpdateSignature(cfg, port, f.secretKey, false, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.refreshSig {
		if err := builder.UpdateSignature(cfg, port, f.secretKey, true, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.checkSig {
		if err := builder.CheckSignature(cfg, port, f.publicKey, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.downloadOnly {
		if _, err := builder.DownloadOnly(cfg, port, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.extractOnly {
		if _, err := builder.ExtractOnly(cfg, port, builder.Options{
			IgnoreMD5Sum:    f.ignoreMD5,
			IgnoreSignature: f.ignoreSig,
			PublicKey:       f.publicKey,
			Out:             stdout,
			ErrOut:          stderr,
		}); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}
	if f.upToDate {
		utd, target, err := builder.UpToDate(cfg, port)
		if err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		if utd {
			fmt.Fprintf(stdout, "=======> Package '%s' is up to date.\n", target)
			return 0
		}
		fmt.Fprintf(stdout, "=======> Package '%s' is not up to date.\n", target)
		return 1
	}
	if f.updateFP && !f.force && !f.doInstall && !f.doUpgrade {
		// Upstream `pkgmk -uf` updates .footprint directly from the existing built package archive.
		if err := builder.UpdateFootprintFile(cfg, port, stdout); err != nil {
			fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
			return builder.ExitCodeOf(err)
		}
		return 0
	}

	res, err := builder.Build(cfg, port, builder.Options{
		Force:           f.force,
		IgnoreFootprint: f.ignoreFP,
		IgnoreNew:       f.ignoreNew,
		UpdateFootprint: f.updateFP,
		IgnoreMD5Sum:    f.ignoreMD5,
		IgnoreSignature: f.ignoreSig,
		KeepWork:        f.keepWork,
		NoStrip:         f.noStrip,
		PublicKey:       f.publicKey,
		Out:             stdout,
		ErrOut:          stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "=======> ERROR: %v\n", err)
		return builder.ExitCodeOf(err)
	}

	if f.doInstall || f.doUpgrade {
		name, _, ierr := pkgops.InstallArchive(res.ArchivePath, pkgops.InstallOptions{
			Root:          cfg.Paths.Root,
			PkgaddConf:    cfg.Paths.PkgaddConf,
			Upgrade:       f.doUpgrade,
			Force:         f.force,
			Verbose:       true,
			LogPrefix:     "pkgadd",
			SkipRootCheck: rt.SkipRootCheck,
			SkipLdconfig:  rt.SkipLdconfig,
			Out:           stdout,
			ErrOut:        stderr,
		})
		if ierr != nil {
			fmt.Fprintf(stderr, "=======> ERROR: Installing '%s' failed: %v\n", res.ArchivePath, ierr)
			return builder.ExitInstall
		}
		if !f.doUpgrade {
			_ = state.AddToWorld(cfg.Paths.Root, name)
		}
		fmt.Fprintf(stdout, "=======> Installing '%s' succeeded.\n", res.ArchivePath)
	}
	return 0
}

// --- ports (integrated pure-Go ports engine + 100% CRUX ports(8) parity) ---

func runPorts(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return portscli.Run(ctx, args, stdout, stderr, Version)
}

// --- rejmerge (100% parity with upstream CRUX pkgutils/rejmerge.in) ---

func runRejmerge(args []string, stdout, stderr io.Writer) int {
	root := "/"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(stdout, `usage: rejmerge [options]
options:
  -r, --root <path>   specify alternative installation root
  -v, --version       print version and exit
  -h, --help          print help and exit
`)
			return 0
		case a == "-v" || a == "--version":
			fmt.Fprintf(stdout, "rejmerge (kpm) %s\n", Version)
			return 0
		case a == "-r" || a == "--root":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "rejmerge: option %s requires an argument\n", a)
				return 1
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		default:
			fmt.Fprintf(stderr, "rejmerge: invalid option %s\n", a)
			return 1
		}
	}

	if err := rejected.RunInteractive(rejected.Options{
		Root:        root,
		ProgName:    "rejmerge",
		In:          os.Stdin,
		Out:         stdout,
		ErrOut:      stderr,
		Interactive: true,
	}); err != nil {
		fmt.Fprintf(stderr, "rejmerge: %v\n", err)
		return 1
	}
	return 0
}
