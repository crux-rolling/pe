// Package kpm implements read-only integration with the kpm package manager
// and CRUX Linux's /var/lib/pkg/db package database (including flock locking
// and kpm's numeric-aware version comparison).
package kpmcompat

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const (
	PkgDir = "var/lib/pkg"
	PkgDB  = "var/lib/pkg/db"
)

// InstalledPackage describes a package entry read from <root>/var/lib/pkg/db.
type InstalledPackage struct {
	Name    string
	Version string
	Files   []string
}

// DB represents an in-memory snapshot of <root>/var/lib/pkg/db.
type DB struct {
	Root     string
	Packages map[string]InstalledPackage
}

// OpenReadOnly acquires a CRUX/kpm-compatible non-blocking shared flock on
// <root>/var/lib/pkg, reads <root>/var/lib/pkg/db, and releases the lock.
func OpenReadOnly(root string) (*DB, error) {
	root = normalizeRoot(root)
	dirPath := filepath.Join(root, PkgDir)

	dirFile, err := os.Open(dirPath)
	if err != nil {
		return nil, fmt.Errorf("could not read package db directory %s: %w", dirPath, err)
	}
	defer dirFile.Close()

	if err := syscall.Flock(int(dirFile.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("package database %s is currently locked by another process", dirPath)
		}
		return nil, fmt.Errorf("could not lock package db directory %s: %w", dirPath, err)
	}
	defer syscall.Flock(int(dirFile.Fd()), syscall.LOCK_UN)

	return ReadDBFile(root, filepath.Join(root, PkgDB))
}

// ReadDBFile parses a CRUX/kpm pkgutils plain-text database file.
// Format per record:
//
//	<pkgname>
//	<version>-<release>
//	<file1>
//	<file2>
//	<blank line>
func ReadDBFile(root, dbPath string) (*DB, error) {
	f, err := os.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("could not open package db %s: %w", dbPath, err)
	}
	defer f.Close()

	db := &DB{
		Root:     normalizeRoot(root),
		Packages: make(map[string]InstalledPackage),
	}

	reader := bufio.NewReader(f)
	for {
		name, err := readLine(reader)
		if errors.Is(err, io.EOF) && name == "" {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read %s: %w", dbPath, err)
		}
		if name == "" {
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}

		version, err := readLine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read %s: %w", dbPath, err)
		}

		var files []string
		for {
			file, ferr := readLine(reader)
			if file != "" {
				files = append(files, file)
			}
			if file == "" || errors.Is(ferr, io.EOF) {
				if ferr != nil && !errors.Is(ferr, io.EOF) {
					return nil, fmt.Errorf("read %s: %w", dbPath, ferr)
				}
				break
			}
		}

		// Mirror CRUX pkgutils & kpm/internal/pkgdb: ignore records with zero owned files.
		if len(files) > 0 {
			db.Packages[name] = InstalledPackage{
				Name:    name,
				Version: version,
				Files:   files,
			}
		}
	}

	return db, nil
}

// SortedNames returns all installed package names sorted lexicographically.
func (db *DB) SortedNames() []string {
	if db == nil {
		return nil
	}
	names := make([]string, 0, len(db.Packages))
	for name := range db.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func normalizeRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/"
	}
	return filepath.Clean(root)
}
