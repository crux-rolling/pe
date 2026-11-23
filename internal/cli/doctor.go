package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kryx-linux/pe/internal/config"
	"github.com/kryx-linux/pe/internal/ports"
	"github.com/kryx-linux/pe/internal/repo"
)

// cmdDoctor runs a comprehensive system health check for pe, CRUX pkgdb,
// shadowed PATH binaries (e.g. legacy /sbin/pe shadowing /usr/bin/pe),
// configuration files, CA certificates, ports trees, and cached index.json.
func cmdDoctor(cfg config.Config, args []string, stdout, stderr io.Writer, styles uiContext) error {
	_ = args
	failures := 0
	warnings := 0

	ok := func(label, msg string) {
		fmt.Fprintf(stdout, "%s  %-16s %s\n", styles.out.BoldGreen("[OK]"), label, msg)
	}
	warn := func(label, msg string) {
		warnings++
		fmt.Fprintf(stdout, "%s %-16s %s\n", styles.out.BoldYellow("[WARN]"), label, msg)
	}
	fail := func(label, msg string) {
		failures++
		fmt.Fprintf(stderr, "%s %-16s %s\n", styles.err.BoldRed("[FAIL]"), label, msg)
	}

	// 1. Check for duplicate/shadowed binaries across $PATH (catches /sbin/pe shadowing /usr/bin/pe).
	pathEnv := os.Getenv("PATH")
	for _, binName := range []string{"pe", "pe-index", "kryx-src", "gports"} {
		hits := findAllInPath(binName, pathEnv)
		if len(hits) > 1 {
			warn("path:"+binName, fmt.Sprintf("multiple copies in PATH (%s); first shadows later — remove legacy copy (e.g. sudo rm -f %s) and run: hash -r",
				strings.Join(hits, ", "), hits[0]))
		} else if len(hits) == 1 {
			if strings.HasPrefix(hits[0], "/sbin/") || strings.HasPrefix(hits[0], "/usr/local/bin/") {
				warn("path:"+binName, fmt.Sprintf("found at non-standard location %s (expected /usr/bin/%s); run 'make clean-legacy' and 'hash -r'", hits[0], binName))
			} else {
				ok("path:"+binName, hits[0])
			}
		}
	}

	// 2. Check package database (<root>/var/lib/pkg/db) and locking.
	db, err := openDBReadOnly(cfg.Paths.Root)
	if err != nil {
		fail("pkgdb", fmt.Sprintf("cannot open %s/var/lib/pkg/db: %v", strings.TrimRight(cfg.Paths.Root, "/"), err))
	} else {
		ok("pkgdb", fmt.Sprintf("%d installed package(s) in %s", len(db.Packages), filepath.Join(cfg.Paths.Root, "var/lib/pkg/db")))
	}

	// 3. Check /etc/pkgadd.conf.
	pkgaddPath := cfg.Paths.PkgaddConf
	if !filepath.IsAbs(pkgaddPath) {
		pkgaddPath = filepath.Join(cfg.Paths.Root, pkgaddPath)
	}
	if _, err := os.Stat(pkgaddPath); err != nil {
		warn("pkgadd.conf", fmt.Sprintf("%s not found (using implicit install/upgrade-all rules)", pkgaddPath))
	} else {
		ok("pkgadd.conf", pkgaddPath)
	}

	// 4. Check TLS CA bundle.
	caCandidates := []string{os.Getenv("SSL_CERT_FILE"), "/etc/ssl/cert.pem", "/etc/ssl/certs/ca-certificates.crt"}
	foundCA := ""
	for _, c := range caCandidates {
		if c != "" {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() && fi.Size() > 0 {
				foundCA = c
				break
			}
		}
	}
	if foundCA != "" {
		ok("tls-ca", foundCA)
	} else {
		warn("tls-ca", "no CA bundle found at /etc/ssl/cert.pem or SSL_CERT_FILE (HTTPS sync may fail)")
	}

	// 5. Check ports tree(s).
	if entries, err := ports.ListEntries(cfg.Paths.PortsDir, cfg.Paths.Collections); err != nil {
		warn("ports", fmt.Sprintf("%s not readable (%v)", cfg.Paths.PortsDir, err))
	} else {
		ok("ports", fmt.Sprintf("%d port(s) discovered in %s", len(entries), cfg.Paths.PortsDir))
	}

	// 6. Check synced binary index.json.
	idxPath := cfg.LocalIndexPath()
	if idx, err := repo.Load(idxPath); err != nil {
		warn("index.json", fmt.Sprintf("%s not synced yet (run: pe sync)", idxPath))
	} else {
		ok("index.json", fmt.Sprintf("%d package(s) cached in %s", len(idx.Packages), idxPath))
	}

	fmt.Fprintf(stdout, "\nDoctor summary: %d failure(s), %d warning(s)\n", failures, warnings)
	if failures > 0 {
		return fmt.Errorf("doctor detected %d failure(s)", failures)
	}
	return nil
}

func findAllInPath(name, pathEnv string) []string {
	if pathEnv == "" {
		return nil
	}
	seen := make(map[string]bool)
	var hits []string
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		full := filepath.Join(dir, name)
		realPath := full
		if resolved, err := filepath.EvalSymlinks(full); err == nil {
			realPath = resolved
		}
		if seen[realPath] {
			continue
		}
		if fi, err := os.Stat(full); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			seen[realPath] = true
			hits = append(hits, full)
		}
	}
	return hits
}
