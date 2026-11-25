package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kryx-linux/kpm/internal/pkgdb"
)

// runPrtGet implements a lightweight CRUX prt-get compatibility shim for Pkgfiles
// (320+ CRUX opt/xorg/contrib Pkgfiles call `prt-get isinst <pkg...>` during build()
// to enable optional features like libinput/xwayland in wlroots).
func runPrtGet(args []string, stdout, stderr io.Writer) int {
	root := "/"
	quiet := false
	showVersion := false
	var pos []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help" || a == "help":
			fmt.Fprint(stdout, `usage: prt-get [options] <command> [args...]
KryX kpm compatibility shim for CRUX Pkgfiles (reads /var/lib/pkg/db).

commands:
  isinst <pkg...>     exit 0 if all listed packages are installed, else 1
  current <pkg>       print installed version of <pkg>
  listinst [-v]       list installed packages
  version             print version and exit

options:
  --install-root=<p>  alternate root directory (default: /)
  -q, --quiet         suppress output for isinst
  -v, --version       print version (or show versions with listinst)
`)
			return 0
		case a == "--version" || a == "version":
			fmt.Fprintf(stdout, "prt-get (kpm) %s\n", Version)
			return 0
		case a == "-v":
			showVersion = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case strings.HasPrefix(a, "--install-root="):
			root = strings.TrimPrefix(a, "--install-root=")
		case a == "--install-root" || a == "-r" || a == "--root":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "prt-get: option %s requires an argument\n", a)
				return 1
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="), strings.HasPrefix(a, "--config-file="):
			// Accepted for CRUX prt-get compatibility; ignored by shim.
		case a == "--config" || a == "--config-file":
			if i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "-"):
			// Ignore benign formatting flags passed by some scripts (e.g. --regex).
		default:
			pos = append(pos, a)
		}
	}

	if len(pos) == 0 {
		if showVersion {
			fmt.Fprintf(stdout, "prt-get (kpm) %s\n", Version)
			return 0
		}
		fmt.Fprintln(stderr, "prt-get: missing command (try 'prt-get --help')")
		return 1
	}

	cmd := pos[0]
	rest := pos[1:]

	db, err := pkgdb.Open(root)
	if err != nil {
		fmt.Fprintf(stderr, "prt-get: failed to read package db under %s: %v\n", root, err)
		return 1
	}

	switch cmd {
	case "isinst":
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "prt-get: isinst requires at least one package name")
			return 1
		}
		allInstalled := true
		for _, name := range rest {
			if _, ok := db.Packages[name]; ok {
				if !quiet {
					fmt.Fprintf(stdout, "package %s is installed\n", name)
				}
			} else {
				allInstalled = false
				if !quiet {
					fmt.Fprintf(stdout, "package %s is not installed\n", name)
				}
			}
		}
		if !allInstalled {
			return 1
		}
		return 0

	case "current":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "prt-get: current requires exactly one package name")
			return 1
		}
		if pkg, ok := db.Packages[rest[0]]; ok {
			fmt.Fprintln(stdout, pkg.Version)
			return 0
		}
		fmt.Fprintf(stdout, "Package %s not installed\n", rest[0])
		return 1

	case "listinst":
		names := make([]string, 0, len(db.Packages))
		for name := range db.Packages {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if showVersion {
				fmt.Fprintf(stdout, "%s %s\n", name, db.Packages[name].Version)
			} else {
				fmt.Fprintln(stdout, name)
			}
		}
		return 0

	default:
		fmt.Fprintf(stderr, "prt-get: unsupported shim command %q (use kpm directly)\n", cmd)
		return 1
	}
}
