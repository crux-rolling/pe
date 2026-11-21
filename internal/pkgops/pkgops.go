// Package pkgops implements CRUX pkgadd and pkgrm package transactions
// combining pkgdb locking/rules/conflicts with archive extraction.
package pkgops

import (
	"fmt"
	"io"
	"os"

	"github.com/ezke5/pe/internal/archive"
	"github.com/ezke5/pe/internal/pkgdb"
)

// DefaultMaxConflictsDisplayed limits conflict lines printed to stderr unless Verbose is set.
const DefaultMaxConflictsDisplayed = 20

// InstallOptions controls a CRUX pkgadd-compatible archive install or upgrade.
type InstallOptions struct {
	Root          string
	PkgaddConf    string
	Upgrade       bool
	Force         bool
	Verbose       bool
	SkipRootCheck bool
	SkipLdconfig  bool
	Out           io.Writer
	ErrOut        io.Writer
}

// RemoveOptions controls a CRUX pkgrm-compatible package removal.
type RemoveOptions struct {
	Root          string
	SkipRootCheck bool
	SkipLdconfig  bool
	ErrOut        io.Writer
}

// InstallArchive installs or upgrades a CRUX .pkg.tar.<ext> archive into opts.Root,
// faithfully following CRUX pkgadd.cc policy.
func InstallArchive(archivePath string, opts InstallOptions) (pkgName, version string, err error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.ErrOut == nil {
		opts.ErrOut = io.Discard
	}
	if !opts.SkipRootCheck && os.Geteuid() != 0 {
		return "", "", fmt.Errorf("only root can install/upgrade packages")
	}

	if err := pkgdb.EnsureInitialized(opts.Root); err != nil {
		return "", "", err
	}

	lock, err := pkgdb.AcquireLock(opts.Root, true)
	if err != nil {
		return "", "", err
	}
	defer lock.Release()

	db, err := pkgdb.Open(opts.Root)
	if err != nil {
		return "", "", err
	}

	name, info, err := archive.Open(archivePath)
	if err != nil {
		return "", "", err
	}

	rules, err := pkgdb.ReadRules(opts.Root, opts.PkgaddConf)
	if err != nil {
		return "", "", err
	}

	installed := db.FindPkg(name)
	if installed && !opts.Upgrade {
		return "", "", fmt.Errorf("package %s already installed (use --upgrade to upgrade)", name)
	}
	if !installed && opts.Upgrade {
		return "", "", fmt.Errorf("package %s not previously installed (skip --upgrade to install)", name)
	}

	nonInstallFiles := pkgdb.ApplyInstallRules(&info, rules)
	conflicts := db.FindConflicts(name, info)

	if len(conflicts) > 0 {
		if opts.Force {
			var forceKeep map[string]struct{}
			if opts.Upgrade {
				forceKeep = pkgdb.MakeKeepListFromSlice(conflicts, rules)
			}
			db.RemoveFiles(conflicts, forceKeep, opts.ErrOut)
		} else {
			limit := len(conflicts)
			if !opts.Verbose && limit > DefaultMaxConflictsDisplayed {
				limit = DefaultMaxConflictsDisplayed
			}
			for _, f := range conflicts[:limit] {
				fmt.Fprintln(opts.ErrOut, f)
			}
			if len(conflicts) > limit {
				fmt.Fprintf(opts.ErrOut, "... and %d more conflicting file(s) (%d total; pass --verbose to list all)\n",
					len(conflicts)-limit, len(conflicts))
			}
			return "", "", fmt.Errorf("%d conflicting file(s) already exist on disk (use --force to overwrite)", len(conflicts))
		}
	}

	var keepList map[string]struct{}
	if opts.Upgrade {
		keepList = pkgdb.MakeKeepList(info.Files, rules)
		db.RemovePkg(name, keepList, opts.ErrOut)
	}

	db.AddPkg(name, info)
	if err := db.Commit(); err != nil {
		return "", "", err
	}

	if err := archive.Extract(archivePath, opts.Root, keepList, nonInstallFiles, installed, opts.Out, opts.ErrOut); err != nil {
		if !installed {
			db.RemovePkg(name, nil, opts.ErrOut)
			_ = db.Commit()
			return "", "", fmt.Errorf("install failed for %s: %w", name, err)
		}
	}

	if !opts.SkipLdconfig {
		pkgdb.RunLdconfig(opts.Root, opts.ErrOut)
	}
	return name, info.Version, nil
}

// RemovePackage removes an installed package from opts.Root following CRUX pkgrm.cc policy.
func RemovePackage(name string, opts RemoveOptions) error {
	if opts.ErrOut == nil {
		opts.ErrOut = io.Discard
	}
	if !opts.SkipRootCheck && os.Geteuid() != 0 {
		return fmt.Errorf("only root can remove packages")
	}

	lock, err := pkgdb.AcquireLock(opts.Root, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	db, err := pkgdb.Open(opts.Root)
	if err != nil {
		return err
	}
	if !db.FindPkg(name) {
		return fmt.Errorf("package %s not installed", name)
	}

	db.RemovePkg(name, nil, opts.ErrOut)
	if !opts.SkipLdconfig {
		pkgdb.RunLdconfig(opts.Root, opts.ErrOut)
	}
	return db.Commit()
}
