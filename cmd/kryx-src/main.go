// Command kryx-src is a unified xbps-src-style source build, linting, and
// release-staging machine for KryX/CRUX Linux ports trees.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kryx-linux/kpm/internal/cli"
	"github.com/kryx-linux/kpm/internal/signing"
	"github.com/kryx-linux/kpm/internal/srcmachine"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Fprintf(stdout, "kryx-src %s\n", cli.Version)
		return 0
	case "pkg", "build":
		if err := cmdPkg(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	case "lint", "check":
		if err := cmdLint(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	case "list", "ls":
		if err := cmdList(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	case "stage", "index":
		if err := cmdStage(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	case "sign":
		if err := cmdSign(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	case "plan":
		if err := cmdPlan(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	case "rewrite-urls":
		if err := cmdRewriteURLs(args[1:], stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "kryx-src: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "kryx-src: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: kryx-src <command> [options] [args...]

Unified xbps-src-style source build, linting, and release-staging machine for KryX/CRUX ports.

Commands:
  pkg, build       Resolve targets/deps via canonical registry and build .pkg.tar.<ext> archives
  plan             Generate topological GitHub Actions matrix JSON (--max-ports, --chunk-size)
  lint, check      Validate Pkgfiles, dir/name alignment, duplicate names, deps, and cycles
  list, ls         List discovered ports (relative path, Pkgfile name, version)
  stage, index     Generate/update index.json and optional GitHub-safe release upload assets
  sign             Sign index.json with a signify/Ed25519 private key (-> index.json.sig)
  rewrite-urls     Rewrite index.json URLs from GitHub release assets JSON and verify built archives
  version          Print version and exit
  help             Show this help message

Ports directory resolution priority (no hardcoded /usr/ports requirement):
  1. --ports-dir / -P / --ports <path>
  2. $KRYX_PORTS_DIR or $KPM_PORTS_DIR
  3. [paths] ports_dir in kpm.ini
  4. Workspace auto-detection (./Pkgfile, ./ports, or ./core|opt|xorg|contrib|rolling)
  5. /usr/ports (only if present on disk)

Run 'kryx-src <command> --help' for command-specific flags.
`)
}

func bindPortsFlags(fs *flag.FlagSet) *string {
	var portsDir string
	fs.StringVar(&portsDir, "ports-dir", "", "ports tree directory or single-port repo root")
	fs.StringVar(&portsDir, "P", "", "shorthand for --ports-dir")
	fs.StringVar(&portsDir, "ports", "", "alias for --ports-dir")
	return &portsDir
}

func cmdPkg(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pkg", flag.ContinueOnError)
	fs.SetOutput(stderr)

	portsDir := bindPortsFlags(fs)
	fromGit := fs.String("from-git", "", "clone and build a Nix-like source repo directly (<url>[@ref])")
	collections := fs.String("collections", "", "comma-separated collection priority order (empty = auto-discover)")
	repoHint := fs.String("port-repo", "", "optional source repo slug hint (e.g. kryx-linux/kernel)")
	workRoot := fs.String("work-root", "", "isolated build work root (contains cache/sources/work/packages)")
	outDir := fs.String("out-dir", "", "output directory for built .pkg.tar.<ext> archives")
	root := fs.String("root", "/", "target CRUX rootfs for installing build dependencies")
	cfgPath := fs.String("config", "", "optional base kpm.ini configuration file")
	repository := fs.String("repo", "kryx-linux/ports", "binary repository slug for index.json dep reuse")
	releaseTag := fs.String("tag", "rolling-x86_64", "binary release tag")
	arch := fs.String("arch", "x86_64", "target architecture")
	jobs := fs.Int("jobs", 0, "parallel build jobs (0 = nproc)")
	compression := fs.String("compression", "zst", "package archive compression (zst, xz, gz, bz2, lz)")
	cflags := fs.String("cflags", "", "override CFLAGS")
	cxxflags := fs.String("cxxflags", "", "override CXXFLAGS")
	syncIndex := fs.Bool("sync-index", false, "sync remote binary index.json before resolving build deps")
	deps := fs.Bool("deps", false, "resolve and satisfy recursive Pkgfile dependencies")
	sourceDeps := fs.Bool("source-deps", false, "build missing dependencies from source instead of binary index")
	force := fs.Bool("force", false, "force rebuild even if archive is up to date")
	install := fs.Bool("install", false, "install built target package(s) into --root")
	ignoreFP := fs.Bool("ignore-footprint", false, "skip .footprint verification")
	ignoreNew := fs.Bool("ignore-new", false, "allow NEW files during .footprint check")
	updateFP := fs.Bool("update-footprint", false, "regenerate .footprint from build output")
	ignoreMD5 := fs.Bool("ignore-md5sum", false, "skip .md5sum verification")
	keepWork := fs.Bool("keep-work", false, "preserve work directory after build")
	noStrip := fs.Bool("no-strip", false, "disable ELF stripping")
	dryRun := fs.Bool("dry-run", false, "resolve and print build plan without compiling")
	colorMode := fs.String("color", "auto", "color output mode: auto, always, never")

	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	effectivePorts := strings.TrimSpace(*portsDir)
	if strings.TrimSpace(*fromGit) != "" {
		repoURL := strings.TrimSpace(*fromGit)
		ref := ""
		if at := strings.LastIndex(repoURL, "@"); at > strings.Index(repoURL, "://")+3 {
			ref = repoURL[at+1:]
			repoURL = repoURL[:at]
		}
		cloneBase := *workRoot
		if cloneBase == "" {
			cloneBase = os.TempDir()
		}
		cloneDir := filepath.Join(cloneBase, "from-git-src")
		_ = os.RemoveAll(cloneDir)
		cloneArgs := []string{"clone", "--depth=1"}
		if ref != "" {
			cloneArgs = append(cloneArgs, "--branch", ref)
		}
		cloneArgs = append(cloneArgs, repoURL, cloneDir)
		cmd := exec.Command("git", cloneArgs...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("clone --from-git %s: %w", *fromGit, err)
		}
		effectivePorts = cloneDir
		if len(targets) == 0 {
			targets = []string{"."}
		}
	} else {
		resolved, err := srcmachine.ResolvePortsDir(effectivePorts, *cfgPath, "")
		if err != nil {
			return err
		}
		effectivePorts = resolved
	}
	if len(targets) == 0 {
		return fmt.Errorf("usage: kryx-src pkg [options] <port|collection|all...>")
	}

	res, err := srcmachine.Build(srcmachine.BuildOptions{
		PortsDir:        effectivePorts,
		Collections:     splitCSV(*collections),
		Targets:         targets,
		RepoHint:        *repoHint,
		WorkRoot:        *workRoot,
		OutDir:          *outDir,
		Root:            *root,
		BaseConfigPath:  *cfgPath,
		Repository:      *repository,
		ReleaseTag:      *releaseTag,
		Arch:            *arch,
		Jobs:            *jobs,
		Compression:     *compression,
		CFLAGS:          *cflags,
		CXXFLAGS:        *cxxflags,
		SyncBinaryIndex: *syncIndex,
		WithDeps:        *deps,
		SourceDeps:      *sourceDeps,
		Force:           *force,
		Install:         *install,
		IgnoreFootprint: *ignoreFP,
		IgnoreNew:       *ignoreNew,
		UpdateFootprint: *updateFP,
		IgnoreMD5Sum:    *ignoreMD5,
		IgnoreSignature: true,
		KeepWork:        *keepWork,
		NoStrip:         *noStrip,
		DryRun:          *dryRun,
		ColorMode:       *colorMode,
		Out:             stdout,
		ErrOut:          stderr,
	})
	if err != nil {
		return err
	}
	if !*dryRun && len(res.BuiltArchives) == 0 {
		return fmt.Errorf("build produced no .pkg.tar.* archives")
	}
	for _, a := range res.BuiltArchives {
		fmt.Fprintf(stdout, "BUILT %s\n", a)
	}
	return nil
}

func cmdLint(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	portsDir := bindPortsFlags(fs)
	cfgPath := fs.String("config", "", "optional kpm.ini configuration file")
	collections := fs.String("collections", "", "comma-separated collection list")
	strict := fs.Bool("strict", false, "treat warnings (e.g. dir!=name, missing deps) as errors")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedPorts, err := srcmachine.ResolvePortsDir(*portsDir, *cfgPath, "")
	if err != nil {
		return err
	}
	reg, err := srcmachine.LoadRegistry(resolvedPorts, splitCSV(*collections))
	if err != nil {
		return err
	}

	var entries []srcmachine.Entry
	if fs.NArg() > 0 {
		entries, err = reg.ExpandTargets(fs.Args(), "")
		if err != nil {
			return err
		}
	}

	report := srcmachine.Lint(reg, entries)
	for _, iss := range report.Issues {
		if iss.Severity == srcmachine.SeverityError {
			fmt.Fprintln(stderr, iss.String())
		} else {
			fmt.Fprintln(stdout, iss.String())
		}
	}
	fmt.Fprintf(stdout, "Linted %d port(s): %d error(s), %d warning(s)\n",
		report.Checked, report.ErrorCount(), report.WarningCount())

	if report.ErrorCount() > 0 || (*strict && report.WarningCount() > 0) {
		return fmt.Errorf("lint failed with %d error(s) and %d warning(s)", report.ErrorCount(), report.WarningCount())
	}
	return nil
}

func cmdList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	portsDir := bindPortsFlags(fs)
	cfgPath := fs.String("config", "", "optional kpm.ini configuration file")
	collections := fs.String("collections", "", "comma-separated collection list")
	asJSON := fs.Bool("json", false, "output structured JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedPorts, err := srcmachine.ResolvePortsDir(*portsDir, *cfgPath, "")
	if err != nil {
		return err
	}
	reg, err := srcmachine.LoadRegistry(resolvedPorts, splitCSV(*collections))
	if err != nil {
		return err
	}
	entries := reg.Entries
	if fs.NArg() > 0 {
		entries, err = reg.ExpandTargets(fs.Args(), "")
		if err != nil {
			return err
		}
	}

	if *asJSON {
		type jsonEntry struct {
			RelDir      string   `json:"rel_dir"`
			Name        string   `json:"name"`
			Version     string   `json:"version"`
			Release     string   `json:"release"`
			Collection  string   `json:"collection,omitempty"`
			Description string   `json:"description,omitempty"`
			Depends     []string `json:"depends,omitempty"`
			MakeDepends []string `json:"make_depends,omitempty"`
		}
		out := make([]jsonEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, jsonEntry{
				RelDir:      e.RelDir,
				Name:        e.Port.Name,
				Version:     e.Port.Version,
				Release:     e.Port.Release,
				Collection:  e.Port.Collection,
				Description: e.Port.Description,
				Depends:     e.Port.Depends,
				MakeDepends: e.Port.MakeDepends,
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	for _, e := range entries {
		if e.Base != "" && e.Port.Name != "" && e.Base != e.Port.Name && !reg.IsSinglePort {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", e.RelDir, e.Port.Name, e.Port.FullVersion())
		} else {
			fmt.Fprintf(stdout, "%s\t%s\n", e.RelDir, e.Port.FullVersion())
		}
	}
	return nil
}

func cmdStage(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("stage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	packagesDir := fs.String("packages", ".", "directory containing built .pkg.tar.<ext> archives")
	portsDir := bindPortsFlags(fs)
	cfgPath := fs.String("config", "", "optional kpm.ini configuration file")
	indexPath := fs.String("index", "index.json", "path to index.json to create or update")
	uploadDir := fs.String("upload-dir", "", "optional directory to stage GitHub-safe ('#' -> '.') upload copies")
	baseURL := fs.String("base-url", "", "base download URL for index entries")
	arch := fs.String("arch", "x86_64", "package architecture")
	prune := fs.Bool("prune", false, "remove index entries missing from --packages")

	if err := fs.Parse(args); err != nil {
		return err
	}

	effPorts := strings.TrimSpace(*portsDir)
	if effPorts != "" {
		resolved, err := srcmachine.ResolvePortsDir(effPorts, *cfgPath, "")
		if err != nil {
			return err
		}
		effPorts = resolved
	} else if env := strings.TrimSpace(os.Getenv("KRYX_PORTS_DIR")); env != "" {
		effPorts = env
	} else if env := strings.TrimSpace(os.Getenv("KPM_PORTS_DIR")); env != "" {
		effPorts = env
	}

	res, err := srcmachine.Stage(srcmachine.StageOptions{
		PackagesDir: *packagesDir,
		PortsDir:    effPorts,
		IndexPath:   *indexPath,
		UploadDir:   *uploadDir,
		BaseURL:     *baseURL,
		Arch:        *arch,
		Prune:       *prune,
		ErrOut:      stderr,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Staged %d package(s) into %s\n", res.IndexedCount, *indexPath)
	return nil
}

func cmdRewriteURLs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rewrite-urls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	indexPath := fs.String("index", "index.json", "path to index.json to rewrite")
	assetsJSON := fs.String("assets-json", "", "path to JSON file from `gh release view --json assets`")
	packagesDir := fs.String("packages", "", "optional built packages dir to verify URL resolution")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*assetsJSON) == "" {
		return fmt.Errorf("--assets-json is required")
	}
	if err := srcmachine.RewriteReleaseURLs(*indexPath, *assetsJSON, *packagesDir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Verified and updated download URLs in %s\n", *indexPath)
	return nil
}

func cmdSign(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyPath := fs.String("key", "", "path to signify or kryx-ed25519 private key file")
	filePath := fs.String("file", "index.json", "file to sign (e.g. index.json)")
	outSig := fs.String("out", "", "output signature path (default: <file>.sig)")
	signifyFmt := fs.Bool("signify", false, "emit OpenBSD signify-compatible detached signature")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyPath) == "" {
		return fmt.Errorf("--key is required")
	}
	sigPath := *outSig
	if sigPath == "" {
		sigPath = *filePath + ".sig"
	}
	var keyID string
	var err error
	if *signifyFmt {
		keyID, err = signing.SignFileSignify(*keyPath, *filePath, sigPath)
	} else {
		keyID, err = signing.SignFile(*keyPath, *filePath, sigPath)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signed %s -> %s (key-id %s)\n", *filePath, sigPath, keyID)
	return nil
}

func cmdPlan(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	portsDir := bindPortsFlags(fs)
	cfgPath := fs.String("config", "", "optional kpm.ini configuration file")
	collections := fs.String("collections", "", "comma-separated collection list")
	repoHint := fs.String("port-repo", "", "optional source repo slug hint")
	maxPorts := fs.Int("max-ports", 20, "maximum number of ports to include per workflow run")
	chunkSize := fs.Int("chunk-size", 1, "number of ports per matrix job")
	compact := fs.Bool("compact", false, "emit single-line JSON suitable for GITHUB_OUTPUT")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targets := fs.Args()
	if len(targets) == 0 {
		return fmt.Errorf("usage: kryx-src plan [options] <port|collection|all...>")
	}
	resolvedPorts, err := srcmachine.ResolvePortsDir(*portsDir, *cfgPath, "")
	if err != nil {
		return err
	}
	reg, err := srcmachine.LoadRegistry(resolvedPorts, splitCSV(*collections))
	if err != nil {
		return err
	}
	plan, err := srcmachine.BuildMatrixPlan(reg, targets, *repoHint, *maxPorts, *chunkSize)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	if !*compact {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(plan)
}

func splitCSV(raw string) []string {
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
