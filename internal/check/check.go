// Package check implements system & ABI health diagnostics for kpm:
//   - CheckBroken (`kpm check --broken`): pure-Go ELF revdep scanner detecting missing .so dependencies
//   - CheckFiles (`kpm check --files [<pkg...>]`): package database filesystem integrity verification
package check

import (
	"bufio"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kryx-linux/kpm/internal/pkgdb"
)

// BrokenFile describes an installed ELF binary/library with one or more unresolved DT_NEEDED libraries.
type BrokenFile struct {
	Package     string
	Path        string // Relative path in pkgdb (e.g. "usr/bin/foo")
	MissingLibs []string
}

// BrokenReport summarizes `kpm check --broken` results.
type BrokenReport struct {
	ScannedELF       int
	BrokenFiles      []BrokenFile
	AffectedPackages []string
}

// FileIssue describes a missing or mismatched file recorded in /var/lib/pkg/db.
type FileIssue struct {
	Package string
	Path    string
	Reason  string
}

// FilesReport summarizes `kpm check --files` results.
type FilesReport struct {
	CheckedPackages int
	CheckedFiles    int
	Issues          []FileIssue
}

// CheckBroken scans all installed ELF binaries and shared libraries recorded in
// <root>/var/lib/pkg/db using Go's debug/elf package to detect missing dynamic
// library dependencies (.so ABI breakages), mirroring CRUX revdep.
func CheckBroken(root string, filterPkgs []string) (BrokenReport, error) {
	absRoot, err := filepath.Abs(normalizeRoot(root))
	if err != nil {
		return BrokenReport{}, err
	}
	lock, err := pkgdb.AcquireLock(absRoot, false)
	if err != nil {
		return BrokenReport{}, err
	}
	defer lock.Release()

	db, err := pkgdb.Open(absRoot)
	if err != nil {
		return BrokenReport{}, err
	}

	targetPkgs := db.SortedNames()
	if len(filterPkgs) > 0 {
		targetPkgs = make([]string, 0, len(filterPkgs))
		for _, p := range filterPkgs {
			if !db.FindPkg(p) {
				return BrokenReport{}, fmt.Errorf("package %q is not installed", p)
			}
			targetPkgs = append(targetPkgs, p)
		}
		sort.Strings(targetPkgs)
	}

	// Build dynamic linker search directories from <root>/etc/ld.so.conf and standard paths.
	sysDirs := collectLdSearchDirs(absRoot)

	// Cache ELF class of discovered shared libraries on disk so repeated lookups are O(1).
	libClassCache := make(map[string]elf.Class)
	resolveLib := func(soname string, wantClass elf.Class, searchDirs []string) bool {
		if strings.Contains(soname, "/") {
			cand := soname
			if filepath.IsAbs(cand) && absRoot != "/" {
				cand = filepath.Join(absRoot, strings.TrimPrefix(cand, "/"))
			}
			return elfMatchesClass(cand, wantClass, libClassCache)
		}
		for _, dir := range searchDirs {
			cand := filepath.Join(dir, soname)
			if elfMatchesClass(cand, wantClass, libClassCache) {
				return true
			}
		}
		return false
	}

	var report BrokenReport
	affectedSet := make(map[string]bool)

	for _, pkgName := range targetPkgs {
		info := db.Packages[pkgName]
		for _, relPath := range info.SortedFiles() {
			if strings.HasSuffix(relPath, "/") {
				continue
			}
			fullPath := filepath.Join(absRoot, filepath.FromSlash(relPath))
			fi, err := os.Lstat(fullPath)
			if err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if !hasELFMagic(fullPath) {
				continue
			}
			ef, err := elf.Open(fullPath)
			if err != nil {
				continue
			}
			report.ScannedELF++
			needed, _ := ef.ImportedLibraries()
			rpaths, _ := ef.DynString(elf.DT_RPATH)
			runpaths, _ := ef.DynString(elf.DT_RUNPATH)
			wantClass := ef.Class
			_ = ef.Close()

			if len(needed) == 0 {
				continue
			}

			origin := filepath.Dir(fullPath)
			searchDirs := buildFileSearchDirs(absRoot, origin, rpaths, runpaths, sysDirs)

			var missing []string
			for _, lib := range needed {
				// Ignore virtual kernel DSOs (e.g. linux-vdso.so.1, linux-gate.so.1).
				if strings.HasPrefix(lib, "linux-vdso") || strings.HasPrefix(lib, "linux-gate") {
					continue
				}
				if !resolveLib(lib, wantClass, searchDirs) {
					missing = append(missing, lib)
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				report.BrokenFiles = append(report.BrokenFiles, BrokenFile{
					Package:     pkgName,
					Path:        relPath,
					MissingLibs: missing,
				})
				affectedSet[pkgName] = true
			}
		}
	}

	for p := range affectedSet {
		report.AffectedPackages = append(report.AffectedPackages, p)
	}
	sort.Strings(report.AffectedPackages)
	return report, nil
}

// CheckFiles verifies that every file recorded in <root>/var/lib/pkg/db exists
// on disk with the expected file type and sane permissions.
func CheckFiles(root string, filterPkgs []string) (FilesReport, error) {
	absRoot, err := filepath.Abs(normalizeRoot(root))
	if err != nil {
		return FilesReport{}, err
	}
	lock, err := pkgdb.AcquireLock(absRoot, false)
	if err != nil {
		return FilesReport{}, err
	}
	defer lock.Release()

	db, err := pkgdb.Open(absRoot)
	if err != nil {
		return FilesReport{}, err
	}

	targetPkgs := db.SortedNames()
	if len(filterPkgs) > 0 {
		targetPkgs = make([]string, 0, len(filterPkgs))
		for _, p := range filterPkgs {
			if !db.FindPkg(p) {
				return FilesReport{}, fmt.Errorf("package %q is not installed", p)
			}
			targetPkgs = append(targetPkgs, p)
		}
		sort.Strings(targetPkgs)
	}

	var report FilesReport
	for _, pkgName := range targetPkgs {
		report.CheckedPackages++
		info := db.Packages[pkgName]
		for _, relPath := range info.SortedFiles() {
			report.CheckedFiles++
			isDirEntry := strings.HasSuffix(relPath, "/")
			cleanRel := strings.TrimSuffix(relPath, "/")
			fullPath := filepath.Join(absRoot, filepath.FromSlash(cleanRel))

			if isDirEntry {
				// Follow symlinks for directories (e.g. /bin -> usr/bin).
				fi, err := os.Stat(fullPath)
				if err != nil {
					if os.IsNotExist(err) {
						report.Issues = append(report.Issues, FileIssue{
							Package: pkgName,
							Path:    relPath,
							Reason:  "missing directory",
						})
					} else {
						report.Issues = append(report.Issues, FileIssue{
							Package: pkgName,
							Path:    relPath,
							Reason:  fmt.Sprintf("unreadable directory: %v", err),
						})
					}
					continue
				}
				if !fi.IsDir() {
					report.Issues = append(report.Issues, FileIssue{
						Package: pkgName,
						Path:    relPath,
						Reason:  fmt.Sprintf("expected directory, found %s", fileTypeLabel(fi.Mode())),
					})
				}
				continue
			}

			lfi, err := os.Lstat(fullPath)
			if err != nil {
				if os.IsNotExist(err) {
					report.Issues = append(report.Issues, FileIssue{
						Package: pkgName,
						Path:    relPath,
						Reason:  "missing file",
					})
				} else {
					report.Issues = append(report.Issues, FileIssue{
						Package: pkgName,
						Path:    relPath,
						Reason:  fmt.Sprintf("cannot stat: %v", err),
					})
				}
				continue
			}

			if lfi.IsDir() {
				report.Issues = append(report.Issues, FileIssue{
					Package: pkgName,
					Path:    relPath,
					Reason:  "expected file, found directory",
				})
				continue
			}

			// Check for dangling symlinks (resolving absolute targets within absRoot).
			if lfi.Mode()&os.ModeSymlink != 0 {
				target, rerr := os.Readlink(fullPath)
				if rerr == nil {
					var checkTarget string
					if filepath.IsAbs(target) {
						checkTarget = filepath.Join(absRoot, strings.TrimPrefix(target, "/"))
					} else {
						checkTarget = filepath.Join(filepath.Dir(fullPath), target)
					}
					if _, serr := os.Lstat(checkTarget); os.IsNotExist(serr) {
						report.Issues = append(report.Issues, FileIssue{
							Package: pkgName,
							Path:    relPath,
							Reason:  fmt.Sprintf("broken symlink -> %s", target),
						})
					}
				}
				continue
			}

			// Flag regular files with 0000 permissions (inaccessible even to owner).
			if lfi.Mode().IsRegular() && lfi.Mode().Perm() == 0 {
				report.Issues = append(report.Issues, FileIssue{
					Package: pkgName,
					Path:    relPath,
					Reason:  "invalid permissions (0000)",
				})
			}
		}
	}
	return report, nil
}

func hasELFMagic(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}

func elfMatchesClass(path string, wantClass elf.Class, cache map[string]elf.Class) bool {
	if c, ok := cache[path]; ok {
		return c != elf.ELFCLASSNONE && (wantClass == elf.ELFCLASSNONE || c == wantClass)
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		cache[path] = elf.ELFCLASSNONE
		return false
	}
	ef, err := elf.Open(path)
	if err != nil {
		cache[path] = elf.ELFCLASSNONE
		return false
	}
	c := ef.Class
	_ = ef.Close()
	cache[path] = c
	return c != elf.ELFCLASSNONE && (wantClass == elf.ELFCLASSNONE || c == wantClass)
}

func buildFileSearchDirs(absRoot, origin string, rpaths, runpaths, sysDirs []string) []string {
	var dirs []string
	seen := make(map[string]bool)
	addDir := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" {
			return
		}
		d = strings.ReplaceAll(d, "${ORIGIN}", origin)
		d = strings.ReplaceAll(d, "$ORIGIN", origin)
		if filepath.IsAbs(d) && absRoot != "/" && !strings.HasPrefix(d, absRoot+string(os.PathSeparator)) && d != absRoot {
			d = filepath.Join(absRoot, strings.TrimPrefix(d, "/"))
		}
		clean := filepath.Clean(d)
		if !seen[clean] {
			seen[clean] = true
			dirs = append(dirs, clean)
		}
	}

	// Dynamic linker search order:
	// 1. DT_RPATH (only when DT_RUNPATH is absent)
	if len(runpaths) == 0 {
		for _, rp := range rpaths {
			for _, part := range strings.Split(rp, ":") {
				addDir(part)
			}
		}
	}
	// 2. LD_LIBRARY_PATH (only when inspecting host root /)
	if absRoot == "/" {
		if ldEnv := os.Getenv("LD_LIBRARY_PATH"); ldEnv != "" {
			for _, part := range strings.Split(ldEnv, ":") {
				addDir(part)
			}
		}
	}
	// 3. DT_RUNPATH
	for _, rp := range runpaths {
		for _, part := range strings.Split(rp, ":") {
			addDir(part)
		}
	}
	// 4. ld.so.conf & default system library dirs
	for _, sd := range sysDirs {
		addDir(sd)
	}
	return dirs
}

func collectLdSearchDirs(absRoot string) []string {
	var dirs []string
	seen := make(map[string]bool)
	visitedConf := make(map[string]bool)

	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		full := raw
		if filepath.IsAbs(raw) && absRoot != "/" {
			full = filepath.Join(absRoot, strings.TrimPrefix(raw, "/"))
		} else if !filepath.IsAbs(raw) {
			full = filepath.Join(absRoot, raw)
		}
		clean := filepath.Clean(full)
		if !seen[clean] {
			seen[clean] = true
			dirs = append(dirs, clean)
		}
	}

	var parseConf func(confPath string)
	parseConf = func(confPath string) {
		clean := filepath.Clean(confPath)
		if visitedConf[clean] {
			return
		}
		visitedConf[clean] = true
		f, err := os.Open(clean)
		if err != nil {
			return
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if idx := strings.IndexByte(line, '#'); idx >= 0 {
				line = strings.TrimSpace(line[:idx])
			}
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "include" {
				for _, pat := range fields[1:] {
					var globPat string
					if filepath.IsAbs(pat) {
						globPat = filepath.Join(absRoot, strings.TrimPrefix(pat, "/"))
					} else {
						globPat = filepath.Join(filepath.Dir(clean), pat)
					}
					matches, _ := filepath.Glob(globPat)
					sort.Strings(matches)
					for _, m := range matches {
						parseConf(m)
					}
				}
				continue
			}
			if fields[0] == "hwcap" {
				continue
			}
			add(fields[0])
		}
	}

	parseConf(filepath.Join(absRoot, "etc", "ld.so.conf"))
	for _, def := range []string{"/lib", "/usr/lib", "/lib64", "/usr/lib64"} {
		add(def)
	}
	return dirs
}

func fileTypeLabel(m os.FileMode) string {
	switch {
	case m.IsRegular():
		return "regular file"
	case m.IsDir():
		return "directory"
	case m&os.ModeSymlink != 0:
		return "symlink"
	case m&os.ModeNamedPipe != 0:
		return "fifo"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	default:
		return "special file"
	}
}

func normalizeRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/"
	}
	return root
}
