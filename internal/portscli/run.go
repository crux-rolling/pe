// Package portscli implements the integrated pure-Go `ports` utility inside kpm,
// replacing CRUX ports(8) (and former standalone gports) with zero external CLI
// runtime dependencies while preserving full CRUX /etc/ports/*.{httpup,rsync,git}
// driver fallback compatibility.
package portscli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	kpmconfig "github.com/kryx-linux/kpm/internal/config"
	legacyports "github.com/kryx-linux/kpm/internal/ports"
	"github.com/kryx-linux/kpm/internal/portscli/kpmcompat"
	"github.com/kryx-linux/kpm/internal/portscli/netcheck"
	"github.com/kryx-linux/kpm/internal/portscli/sources"
	portsync "github.com/kryx-linux/kpm/internal/portscli/sync"
	"github.com/kryx-linux/kpm/internal/portscli/tree"
)

const legacySourcesPath = "/etc/gports/sources.list"

type cliOptions struct {
	command           string
	targets           []string
	jobs              int
	configPath        string
	configExplicit    bool
	kpmConfigPath     string
	kpmConfigExplicit bool
	rootOverride      string
	portsDir          string
	portsDirSet       bool
	listCollections   bool
	noNetCheck        bool
	resolvPath        string
}

// Run executes the unified `ports` CLI (invoked via `/usr/bin/ports` symlink or `kpm ports`).
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if ctx == nil {
		ctx = context.Background()
	}

	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "ports: %v\n", err)
		fmt.Fprintf(stderr, "Run 'ports --help' for usage.\n")
		return 1
	}

	if opts.command == "help" {
		printUsage(stdout, version)
		return 0
	}
	if opts.command == "version" {
		fmt.Fprintf(stdout, "ports (kpm) %s\n", version)
		return 0
	}

	kpmCfg, err := sources.LoadKPMConfig(opts.kpmConfigPath, opts.kpmConfigExplicit)
	if err != nil {
		fmt.Fprintf(stderr, "[ERR] %v\n", err)
		return 1
	}
	fullCfg, _ := kpmconfig.Load(opts.kpmConfigPath, false)

	if opts.rootOverride != "" {
		kpmCfg.Root = opts.rootOverride
	}
	if opts.portsDirSet {
		kpmCfg.PortsDir = opts.portsDir
	}

	// Resolve default sources.list when -c / $PORTS_CONFIG was not explicitly passed:
	//   1. <ports_conf_dir>/sources.list (from kpm.ini, if different from /etc/ports)
	//   2. /etc/ports/sources.list
	//   3. Legacy /etc/gports/sources.list (only if no classic CRUX driver configs exist in ports_conf_dir)
	useLegacyDrivers := false
	if !opts.configExplicit {
		if fullCfg.Paths.PortsConfDir != "" {
			confSources := filepath.Join(fullCfg.Paths.PortsConfDir, "sources.list")
			if _, err := os.Stat(confSources); err == nil {
				opts.configPath = confSources
			} else if fullCfg.Paths.PortsDriversDir != "" {
				if entries, derr := os.ReadDir(fullCfg.Paths.PortsDriversDir); derr == nil && len(entries) > 0 {
					useLegacyDrivers = true
				}
			}
		}
		if !useLegacyDrivers {
			if _, err := os.Stat(opts.configPath); os.IsNotExist(err) {
				if _, lerr := os.Stat(legacySourcesPath); lerr == nil {
					opts.configPath = legacySourcesPath
				}
			}
		}
	}

	switch opts.command {
	case "update":
		return cmdUpdate(ctx, opts, kpmCfg, fullCfg, useLegacyDrivers, stdout, stderr)
	case "list":
		if opts.listCollections {
			return cmdStatus(ctx, opts, kpmCfg, stdout, stderr)
		}
		return cmdListPorts(opts, kpmCfg, stdout, stderr)
	case "status":
		return cmdStatus(ctx, opts, kpmCfg, stdout, stderr)
	case "diff":
		return cmdDiff(opts, kpmCfg, stdout, stderr)
	case "search":
		return cmdSearch(opts, kpmCfg, stdout, stderr)
	case "check":
		return cmdCheck(ctx, opts, kpmCfg, stdout, stderr)
	default:
		printUsage(stderr, version)
		return 1
	}
}

func parseArgs(args []string) (cliOptions, error) {
	cfgPath := sources.DefaultConfigPath
	cfgExplicit := false
	if env := strings.TrimSpace(os.Getenv("PORTS_CONFIG")); env != "" {
		cfgPath = env
		cfgExplicit = true
	} else if env := strings.TrimSpace(os.Getenv("GPORTS_CONFIG")); env != "" {
		cfgPath = env
		cfgExplicit = true
	}

	resolvPath := strings.TrimSpace(os.Getenv("PORTS_RESOLV_CONF"))
	if resolvPath == "" {
		resolvPath = strings.TrimSpace(os.Getenv("GPORTS_RESOLV_CONF"))
	}

	opts := cliOptions{
		jobs:              portsync.DefaultMaxWorkers,
		configPath:        cfgPath,
		configExplicit:    cfgExplicit,
		kpmConfigPath:     sources.ResolveKPMConfigPath(""),
		kpmConfigExplicit: strings.TrimSpace(os.Getenv("KPM_CONFIG")) != "",
		resolvPath:        resolvPath,
	}

	for _, envKey := range []string{"PORTS_DIR", "GPORTS_PORTS_DIR", "KPM_PORTS_DIR", "KRYX_PORTS_DIR"} {
		if envPorts := strings.TrimSpace(os.Getenv(envKey)); envPorts != "" {
			opts.portsDir = envPorts
			opts.portsDirSet = true
			break
		}
	}

	if len(args) == 0 {
		return opts, fmt.Errorf("option missing (try 'ports --help')")
	}

	setCmd := func(name, raw string) error {
		if opts.command != "" && opts.command != name {
			return fmt.Errorf("conflicting commands %q and %q", opts.command, raw)
		}
		opts.command = name
		return nil
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			opts.command = "help"
			return opts, nil

		case arg == "-v" || arg == "--version" || arg == "version":
			opts.command = "version"
			return opts, nil

		case arg == "-u" || arg == "--update" || arg == "update":
			if err := setCmd("update", arg); err != nil {
				return opts, err
			}

		case arg == "-l" || arg == "--list" || arg == "list":
			if err := setCmd("list", arg); err != nil {
				return opts, err
			}

		case arg == "-d" || arg == "--diff" || arg == "diff":
			if err := setCmd("diff", arg); err != nil {
				return opts, err
			}

		case arg == "-s" || arg == "--search" || arg == "search":
			if err := setCmd("search", arg); err != nil {
				return opts, err
			}

		case arg == "status":
			if err := setCmd("status", arg); err != nil {
				return opts, err
			}

		case arg == "check":
			if err := setCmd("check", arg); err != nil {
				return opts, err
			}

		case arg == "--collections":
			opts.listCollections = true

		case arg == "--no-netcheck":
			opts.noNetCheck = true

		case arg == "-j" || arg == "--jobs":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s requires a positive integer argument", arg)
			}
			i++
			n, err := parseJobs(args[i])
			if err != nil {
				return opts, err
			}
			opts.jobs = n

		case strings.HasPrefix(arg, "-j="):
			n, err := parseJobs(strings.TrimPrefix(arg, "-j="))
			if err != nil {
				return opts, err
			}
			opts.jobs = n

		case strings.HasPrefix(arg, "--jobs="):
			n, err := parseJobs(strings.TrimPrefix(arg, "--jobs="))
			if err != nil {
				return opts, err
			}
			opts.jobs = n

		case arg == "-c" || arg == "--config":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s requires a file path", arg)
			}
			i++
			opts.configPath = args[i]
			opts.configExplicit = true

		case strings.HasPrefix(arg, "-c="):
			opts.configPath = strings.TrimPrefix(arg, "-c=")
			opts.configExplicit = true

		case strings.HasPrefix(arg, "--config="):
			opts.configPath = strings.TrimPrefix(arg, "--config=")
			opts.configExplicit = true

		case arg == "--kpm-config":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s requires a file path", arg)
			}
			i++
			opts.kpmConfigPath = args[i]
			opts.kpmConfigExplicit = true

		case strings.HasPrefix(arg, "--kpm-config="):
			opts.kpmConfigPath = strings.TrimPrefix(arg, "--kpm-config=")
			opts.kpmConfigExplicit = true

		case arg == "-r" || arg == "--root":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s requires a directory path", arg)
			}
			i++
			opts.rootOverride = args[i]

		case strings.HasPrefix(arg, "-r="):
			opts.rootOverride = strings.TrimPrefix(arg, "-r=")

		case strings.HasPrefix(arg, "--root="):
			opts.rootOverride = strings.TrimPrefix(arg, "--root=")

		case arg == "-p" || arg == "--ports-dir":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s requires a directory path", arg)
			}
			i++
			opts.portsDir = args[i]
			opts.portsDirSet = true

		case strings.HasPrefix(arg, "-p="):
			opts.portsDir = strings.TrimPrefix(arg, "-p=")
			opts.portsDirSet = true

		case strings.HasPrefix(arg, "--ports-dir="):
			opts.portsDir = strings.TrimPrefix(arg, "--ports-dir=")
			opts.portsDirSet = true

		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("invalid option %s", arg)

		default:
			switch opts.command {
			case "update", "list", "diff", "search":
				opts.targets = append(opts.targets, arg)
			case "":
				return opts, fmt.Errorf("unknown command %q", arg)
			default:
				return opts, fmt.Errorf("unexpected argument %q for command %q", arg, opts.command)
			}
		}
	}

	if opts.command == "" {
		return opts, fmt.Errorf("option missing (use -u/update, -l/list, -d/diff, -s/search, status, or check)")
	}
	return opts, nil
}

func parseJobs(val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid worker count %q: must be an integer >= 1", val)
	}
	return n, nil
}

func cmdUpdate(ctx context.Context, opts cliOptions, kpmCfg sources.KPMConfig, fullCfg kpmconfig.Config, useLegacyDrivers bool, stdout, stderr io.Writer) int {
	// Fall back transparently to classic CRUX /etc/ports/*.{httpup,rsync,git} drivers
	// whenever ports_drivers_dir is configured & populated and no sources.list exists there.
	if useLegacyDrivers {
		if err := legacyports.SyncPorts(kpmCfg.PortsDir, fullCfg.Paths.PortsConfDir, fullCfg.Paths.PortsDriversDir, opts.targets, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "ports: %v\n", err)
			return 1
		}
		return 0
	}

	cfgRes := sources.Load(sources.Options{
		ConfigPath: opts.configPath,
		PortsDir:   kpmCfg.PortsDir,
	})
	if !cfgRes.OK() {
		for _, e := range cfgRes.Errors {
			fmt.Fprintf(stderr, "[ERR] %s\n", e.Error())
		}
		return 1
	}

	targets, err := sources.Filter(cfgRes.Sources, opts.targets)
	if err != nil {
		fmt.Fprintf(stderr, "[ERR] %v\n", err)
		return 1
	}

	if !opts.noNetCheck {
		report := netcheck.Run(ctx, targets, netcheck.Options{
			Timeout:        netcheck.DefaultTimeout,
			ResolvConfPath: opts.resolvPath,
		})
		if !report.OK() {
			for _, item := range report.Items {
				if !item.OK {
					fmt.Fprintf(stderr, "[ERR] %s\n", item.Message)
				}
			}
			return 1
		}
	}

	printer := portsync.NewPrinter(stdout, stderr)
	engine := portsync.NewEngine(opts.jobs, printer)
	summary := engine.SyncAll(ctx, targets)

	printer.Info(
		"Summary: %d updated, %d skipped, %d failed (%s)",
		summary.Updated,
		summary.Skipped,
		summary.Failed,
		formatElapsed(summary.Elapsed),
	)

	if summary.Failed > 0 {
		return 1
	}
	return 0
}

func cmdListPorts(opts cliOptions, kpmCfg sources.KPMConfig, stdout, stderr io.Writer) int {
	cols := kpmCfg.Collections
	if len(opts.targets) > 0 {
		cols = opts.targets
	}
	entries, err := tree.ListEntries(kpmCfg.PortsDir, cols)
	if err != nil {
		fmt.Fprintf(stderr, "[ERR] %v\n", err)
		return 1
	}
	for _, e := range entries {
		fmt.Fprintln(stdout, e)
	}
	return 0
}

func cmdStatus(ctx context.Context, opts cliOptions, kpmCfg sources.KPMConfig, stdout, stderr io.Writer) int {
	cfgRes := sources.Load(sources.Options{
		ConfigPath: opts.configPath,
		PortsDir:   kpmCfg.PortsDir,
	})
	if !cfgRes.OK() {
		for _, e := range cfgRes.Errors {
			fmt.Fprintf(stderr, "[ERR] %s\n", e.Error())
		}
		return 1
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "COLLECTION\tPROTOCOL\tDESTINATION\tSTATE")
	for _, src := range cfgRes.Sources {
		st := portsync.InspectCollection(ctx, src)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", src.Collection, src.Protocol, src.DestDir, st.State)
	}
	_ = tw.Flush()
	return 0
}

func cmdDiff(opts cliOptions, kpmCfg sources.KPMConfig, stdout, stderr io.Writer) int {
	cols := kpmCfg.Collections
	if len(opts.targets) > 0 {
		cols = opts.targets
	}

	db, err := kpmcompat.OpenReadOnly(kpmCfg.Root)
	if err != nil {
		fmt.Fprintf(stderr, "[ERR] %v\n", err)
		return 1
	}

	localPorts, err := tree.DiscoverAll(kpmCfg.PortsDir, cols)
	if err != nil {
		fmt.Fprintf(stderr, "[ERR] %v\n", err)
		return 1
	}

	diffs := tree.Diff(db, localPorts, cols)

	// Match upstream CRUX `ports -d` parity: when no collection filter was given,
	// also report installed packages that do not exist in any local port collection.
	if len(opts.targets) == 0 {
		present := make(map[string]bool, len(localPorts))
		for _, p := range localPorts {
			present[p.Name] = true
		}
		for _, name := range db.SortedNames() {
			if !present[name] {
				diffs = append(diffs, tree.DiffEntry{
					Collection: "None",
					Name:       name,
					PortVer:    "MISSING",
					Installed:  db.Packages[name].Version,
				})
			}
		}
		sort.Slice(diffs, func(i, j int) bool {
			if diffs[i].Collection != diffs[j].Collection {
				return diffs[i].Collection < diffs[j].Collection
			}
			return diffs[i].Name < diffs[j].Name
		})
	}

	if len(diffs) == 0 {
		fmt.Fprintln(stdout, "No differences found")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Collection\tName\tPort\tInstalled")
	for _, d := range diffs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Collection, d.Name, d.PortVer, d.Installed)
	}
	_ = tw.Flush()
	return 0
}

func cmdSearch(opts cliOptions, kpmCfg sources.KPMConfig, stdout, stderr io.Writer) int {
	if len(opts.targets) == 0 {
		fmt.Fprintln(stderr, "[ERR] usage: ports -s|--search <query...>")
		return 1
	}
	query := strings.Join(opts.targets, " ")

	localPorts, err := tree.DiscoverAll(kpmCfg.PortsDir, kpmCfg.Collections)
	if err != nil {
		fmt.Fprintf(stderr, "[ERR] %v\n", err)
		return 1
	}

	matches := tree.Search(localPorts, query)
	for _, p := range matches {
		label := p.RelDir
		if label == "" {
			label = p.Name
		}
		if p.Description != "" {
			fmt.Fprintf(stdout, "%s %s - %s\n", label, p.FullVersion(), p.Description)
		} else {
			fmt.Fprintf(stdout, "%s %s\n", label, p.FullVersion())
		}
	}
	return 0
}

func cmdCheck(ctx context.Context, opts cliOptions, kpmCfg sources.KPMConfig, stdout, stderr io.Writer) int {
	healthy := true

	cfgRes := sources.Load(sources.Options{
		ConfigPath: opts.configPath,
		PortsDir:   kpmCfg.PortsDir,
	})

	if cfgRes.OK() {
		fmt.Fprintf(stdout, "[OK]  Config syntax valid (%d file(s), %d collection(s))\n", len(cfgRes.Files), len(cfgRes.Sources))
	} else {
		healthy = false
		for _, e := range cfgRes.Errors {
			fmt.Fprintf(stderr, "[ERR] %s\n", e.Error())
		}
	}

	report := netcheck.Run(ctx, cfgRes.Sources, netcheck.Options{
		Timeout:           netcheck.DefaultTimeout,
		ResolvConfPath:    opts.resolvPath,
		RequireResolvConf: true,
	})

	for _, item := range report.Items {
		if item.OK {
			if item.Elapsed > 0 {
				fmt.Fprintf(stdout, "[OK]  %-24s %s (%s)\n", item.Label, item.Message, formatElapsed(item.Elapsed))
			} else {
				fmt.Fprintf(stdout, "[OK]  %-24s %s\n", item.Label, item.Message)
			}
		} else {
			healthy = false
			fmt.Fprintf(stderr, "[ERR] %s\n", item.Message)
		}
	}

	if !healthy {
		return 1
	}
	return 0
}

func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func printUsage(w io.Writer, version string) {
	fmt.Fprintf(w, `ports (kpm) %s — integrated, zero-dependency pure-Go CRUX/KryX ports utility

Usage:
  ports -u [collection...]          Sync all collections, or only specified ones (alias: update)
  ports -l [collection...]          List available local ports as <collection>/<port> (alias: list)
  ports -d [collection...]          Compare local port versions against installed kpm DB (alias: diff)
  ports -s <query...>               Fast local search across port names and descriptions (alias: search)
  ports status                      Show configured collections, protocols, and local sync state
  ports check                       Lint sources.list and run pre-flight DNS/network diagnostics

Options:
  -j, --jobs <n>                    Max concurrent sync workers (default: %d)
  -c, --config <path>               Path to sources.list (default: %s)
      --kpm-config <path>           Path to kpm.ini (default: $KPM_CONFIG or %s)
  -r, --root <dir>                  Override kpm [paths] root (default: /)
  -p, --ports-dir <dir>             Override kpm [paths] ports_dir (default: %s)
      --collections                 With -l/list, show configured collection sync states (same as status)
      --no-netcheck                 Skip pre-flight DNS/network checks before syncing
  -v, --version                     Print version and exit
  -h, --help                        Show this help message

Configuration:
  Sources file:   %s (+ %s/*.list)
  kpm config:     %s
  Line format:    <protocol> <collection> <url> [destination_dir]
  Protocols:      git, httpup, rsync, tarball (all native pure-Go; zero external binaries)
`, version, portsync.DefaultMaxWorkers, sources.DefaultConfigPath, sources.DefaultKPMConfigPath, sources.DefaultPortsDir, sources.DefaultConfigPath, sources.DefaultDropInDir, sources.DefaultKPMConfigPath)
}
