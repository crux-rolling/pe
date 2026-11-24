// Package pkgdb implements CRUX Linux's pkgutils package database, locking,
// conflict detection, removal, and /etc/pkgadd.conf rule evaluation.
package pkgdb

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	PkgDir       = "var/lib/pkg"
	PkgDB        = "var/lib/pkg/db"
	PkgRejected  = "var/lib/pkg/rejected"
	PkgaddConf   = "etc/pkgadd.conf"
	LdconfigPath = "/sbin/ldconfig"
	LdconfigConf = "etc/ld.so.conf"
)

// PkgInfo mirrors pkgutil::pkginfo_t from CRUX pkgutils.
type PkgInfo struct {
	Version string
	Files   map[string]struct{}
}

// SortedFiles returns the package's file paths sorted lexicographically.
func (p PkgInfo) SortedFiles() []string {
	out := make([]string, 0, len(p.Files))
	for f := range p.Files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// DB represents an opened CRUX package database at Root.
type DB struct {
	Root     string
	Packages map[string]PkgInfo
}

// Lock holds a CRUX-compatible flock on <root>/var/lib/pkg.
type Lock struct {
	dir *os.File
}

// AcquireLock acquires a shared (exclusive=false) or exclusive (exclusive=true)
// non-blocking flock on <root>/var/lib/pkg, matching pkgutils db_lock.
func AcquireLock(root string, exclusive bool) (*Lock, error) {
	root = normalizeRoot(root)
	dirPath := filepath.Join(root, PkgDir)
	f, err := os.Open(dirPath)
	if err != nil {
		return nil, fmt.Errorf("could not read directory %s: %w", dirPath, err)
	}
	how := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("package database is currently locked by another process")
		}
		return nil, fmt.Errorf("could not lock directory %s: %w", dirPath, err)
	}
	return &Lock{dir: f}, nil
}

// Release unlocks and closes the database directory descriptor.
func (l *Lock) Release() error {
	if l == nil || l.dir == nil {
		return nil
	}
	_ = syscall.Flock(int(l.dir.Fd()), syscall.LOCK_UN)
	err := l.dir.Close()
	l.dir = nil
	return err
}

// Open reads <root>/var/lib/pkg/db in CRUX pkgutils format.
func Open(root string) (*DB, error) {
	root = normalizeRoot(root)
	db := &DB{
		Root:     root,
		Packages: make(map[string]PkgInfo),
	}
	filename := filepath.Join(root, PkgDB)
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %w", filename, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		name, err := readLine(reader)
		if errors.Is(err, io.EOF) && name == "" {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("could not read %s: %w", filename, err)
		}
		if name == "" {
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}

		version, err := readLine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("could not read %s: %w", filename, err)
		}

		files := make(map[string]struct{})
		for {
			file, ferr := readLine(reader)
			if file != "" {
				files[file] = struct{}{}
			}
			if file == "" || errors.Is(ferr, io.EOF) {
				if ferr != nil && !errors.Is(ferr, io.EOF) {
					return nil, fmt.Errorf("could not read %s: %w", filename, ferr)
				}
				break
			}
		}
		if len(files) > 0 {
			db.Packages[name] = PkgInfo{
				Version: version,
				Files:   files,
			}
		}
	}
	return db, nil
}

// EnsureInitialized ensures <root>/var/lib/pkg and an empty <root>/var/lib/pkg/db exist.
func EnsureInitialized(root string) error {
	root = normalizeRoot(root)
	dirPath := filepath.Join(root, PkgDir)
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		return fmt.Errorf("create package db directory %s: %w", dirPath, err)
	}
	dbPath := filepath.Join(root, PkgDB)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		if err := os.WriteFile(dbPath, nil, 0o444); err != nil {
			return fmt.Errorf("initialize package db %s: %w", dbPath, err)
		}
	}
	return nil
}

// Commit writes the database atomically using CRUX pkgutils transaction semantics:
// write .incomplete_transaction (0444) -> fsync -> hardlink db to .backup -> rename.
func (db *DB) Commit() error {
	dbFilename := filepath.Join(db.Root, PkgDB)
	dbNew := dbFilename + ".incomplete_transaction"
	dbBak := dbFilename + ".backup"

	if err := os.Remove(dbNew); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not remove %s: %w", dbNew, err)
	}

	f, err := os.OpenFile(dbNew, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o444)
	if err != nil {
		return fmt.Errorf("could not create %s: %w", dbNew, err)
	}
	// Ensure mode 0444 regardless of umask, matching pkgutil.cc creat(..., 0444).
	_ = f.Chmod(0o444)

	bw := bufio.NewWriter(f)
	names := db.SortedNames()
	for _, name := range names {
		info := db.Packages[name]
		if len(info.Files) == 0 {
			continue
		}
		if _, err := bw.WriteString(name + "\n"); err != nil {
			_ = f.Close()
			return fmt.Errorf("could not write %s: %w", dbNew, err)
		}
		if _, err := bw.WriteString(info.Version + "\n"); err != nil {
			_ = f.Close()
			return fmt.Errorf("could not write %s: %w", dbNew, err)
		}
		for _, file := range info.SortedFiles() {
			if _, err := bw.WriteString(file + "\n"); err != nil {
				_ = f.Close()
				return fmt.Errorf("could not write %s: %w", dbNew, err)
			}
		}
		if _, err := bw.WriteString("\n"); err != nil {
			_ = f.Close()
			return fmt.Errorf("could not write %s: %w", dbNew, err)
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("could not write %s: %w", dbNew, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("could not synchronize %s: %w", dbNew, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("could not close %s: %w", dbNew, err)
	}

	if err := os.Remove(dbBak); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not remove %s: %w", dbBak, err)
	}
	if _, statErr := os.Stat(dbFilename); statErr == nil {
		if err := os.Link(dbFilename, dbBak); err != nil {
			return fmt.Errorf("could not create %s: %w", dbBak, err)
		}
	}
	if err := os.Rename(dbNew, dbFilename); err != nil {
		return fmt.Errorf("could not rename %s to %s: %w", dbNew, dbFilename, err)
	}
	return nil
}

// SortedNames returns all installed package names sorted lexicographically.
func (db *DB) SortedNames() []string {
	names := make([]string, 0, len(db.Packages))
	for name := range db.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FindPkg reports whether name is installed in the database.
func (db *DB) FindPkg(name string) bool {
	_, ok := db.Packages[name]
	return ok
}

// AddPkg records or replaces package metadata in memory.
func (db *DB) AddPkg(name string, info PkgInfo) {
	copied := make(map[string]struct{}, len(info.Files))
	for f := range info.Files {
		copied[f] = struct{}{}
	}
	db.Packages[name] = PkgInfo{
		Version: info.Version,
		Files:   copied,
	}
}

// FindConflicts implements pkgutil::db_find_conflicts:
// 1. Files owned by another package in DB.
// 2. Files already existing on disk (lstat).
// 3. Exclude directories (trailing '/').
// 4. If upgrading an installed package, exclude files already owned by this package.
func (db *DB) FindConflicts(name string, info PkgInfo) []string {
	conflicts := make(map[string]struct{})

	// Phase 1: conflicts in database with other packages
	for otherName, otherInfo := range db.Packages {
		if otherName == name {
			continue
		}
		for f := range info.Files {
			if _, ok := otherInfo.Files[f]; ok {
				conflicts[f] = struct{}{}
			}
		}
	}

	// Phase 2: conflicts on filesystem
	for f := range info.Files {
		if _, already := conflicts[f]; already {
			continue
		}
		full := filepath.Join(db.Root, f)
		if _, err := os.Lstat(full); err == nil {
			conflicts[f] = struct{}{}
		}
	}

	// Phase 3: exclude directories
	for f := range conflicts {
		if strings.HasSuffix(f, "/") {
			delete(conflicts, f)
		}
	}

	// Phase 4: if upgrade, exclude files already owned by this package
	if existing, ok := db.Packages[name]; ok {
		for f := range existing.Files {
			delete(conflicts, f)
		}
	}

	out := make([]string, 0, len(conflicts))
	for f := range conflicts {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// RemovePkg implements pkgutil::db_rm_pkg (both normal removal and upgrade removal with keepList).
// When keepList is non-nil (upgrade), ENOTEMPTY errors on directories are silently skipped.
func (db *DB) RemovePkg(name string, keepList map[string]struct{}, errOut io.Writer) {
	info, ok := db.Packages[name]
	if !ok {
		return
	}
	files := make(map[string]struct{}, len(info.Files))
	for f := range info.Files {
		files[f] = struct{}{}
	}
	delete(db.Packages, name)

	// Exclude files in keepList
	for f := range keepList {
		delete(files, f)
	}

	// Exclude files still referenced by other installed packages
	for _, other := range db.Packages {
		for f := range other.Files {
			delete(files, f)
		}
	}

	removePathsReverse(db.Root, files, true, errOut)
}

// RemoveFiles implements pkgutil::db_rm_files for forced conflict removal.
func (db *DB) RemoveFiles(targetFiles []string, keepList map[string]struct{}, errOut io.Writer) {
	files := make(map[string]struct{}, len(targetFiles))
	for _, f := range targetFiles {
		files[f] = struct{}{}
	}
	// Remove references from all packages in DB
	for _, pkg := range db.Packages {
		for f := range files {
			delete(pkg.Files, f)
		}
	}
	// Keep files matching keepList
	for f := range keepList {
		delete(files, f)
	}
	removePathsReverse(db.Root, files, true, errOut)
}

// RunLdconfig executes /sbin/ldconfig -r <root> if <root>/etc/ld.so.conf exists,
// mirroring pkgutil::ldconfig().
func RunLdconfig(root string, errOut io.Writer) {
	root = normalizeRoot(root)
	conf := filepath.Join(root, LdconfigConf)
	if _, err := os.Lstat(conf); err != nil {
		return
	}
	if _, err := os.Stat(LdconfigPath); err != nil {
		return
	}
	cmd := exec.Command(LdconfigPath, "-r", root)
	if out, err := cmd.CombinedOutput(); err != nil && errOut != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			fmt.Fprintf(errOut, "kpm: could not execute %s: %v (%s)\n", LdconfigPath, err, msg)
		} else {
			fmt.Fprintf(errOut, "kpm: could not execute %s: %v\n", LdconfigPath, err)
		}
	}
}

func removePathsReverse(root string, files map[string]struct{}, ignoreNotEmpty bool, errOut io.Writer) {
	sorted := make([]string, 0, len(files))
	for f := range files {
		sorted = append(sorted, f)
	}
	// Sort deepest paths first (files before their parent directories), breaking ties reverse-lexicographically.
	sort.Slice(sorted, func(i, j int) bool {
		di := strings.Count(strings.TrimSuffix(sorted[i], "/"), "/")
		dj := strings.Count(strings.TrimSuffix(sorted[j], "/"), "/")
		if di != dj {
			return di > dj
		}
		dirI := strings.HasSuffix(sorted[i], "/")
		dirJ := strings.HasSuffix(sorted[j], "/")
		if dirI != dirJ {
			return !dirI
		}
		return sorted[i] > sorted[j]
	})

	for _, rel := range sorted {
		full := filepath.Join(root, rel)
		if _, err := os.Lstat(full); err != nil {
			continue
		}
		if err := os.Remove(full); err != nil {
			if ignoreNotEmpty && isNotEmpty(err) {
				continue
			}
			if errOut != nil {
				fmt.Fprintf(errOut, "kpm: could not remove %s: %v\n", full, err)
			}
		}
	}
}

func isNotEmpty(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.ENOTEMPTY || errno == syscall.EEXIST
	}
	return false
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	return line, err
}

func normalizeRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/"
	}
	return filepath.Clean(root)
}
