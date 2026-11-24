// Package state manages kpm persistent package state files under <root>/var/lib/kpm/
// (world explicit-install tracking and hold package version pinning), synchronized
// with CRUX's /var/lib/pkg lock.
package state

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kryx-linux/kpm/internal/pkgdb"
)

const (
	StateDirRel  = "var/lib/kpm"
	WorldFileRel = "var/lib/kpm/world"
	HoldFileRel  = "var/lib/kpm/hold"
)

// WorldPath returns <root>/var/lib/kpm/world.
func WorldPath(root string) string {
	return filepath.Join(normalizeRoot(root), WorldFileRel)
}

// HoldPath returns <root>/var/lib/kpm/hold.
func HoldPath(root string) string {
	return filepath.Join(normalizeRoot(root), HoldFileRel)
}

// LoadWorld reads the set of explicitly installed package names from <root>/var/lib/kpm/world
// under a shared pkgdb lock.
func LoadWorld(root string) (map[string]bool, error) {
	if lock, err := pkgdb.AcquireLock(root, false); err == nil {
		defer lock.Release()
	}
	return LoadList(WorldPath(root))
}

// AddToWorld records one or more package names in <root>/var/lib/kpm/world under an exclusive lock.
func AddToWorld(root string, pkgs ...string) error {
	if len(pkgs) == 0 {
		return nil
	}
	_ = pkgdb.EnsureInitialized(root)
	lock, err := pkgdb.AcquireLock(root, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	set, err := LoadList(WorldPath(root))
	if err != nil {
		return err
	}
	changed := false
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if p != "" && !set[p] {
			set[p] = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return SaveList(WorldPath(root), set)
}

// RemoveFromWorld removes one or more package names from <root>/var/lib/kpm/world under an exclusive lock.
func RemoveFromWorld(root string, pkgs ...string) error {
	if len(pkgs) == 0 {
		return nil
	}
	if _, err := os.Stat(WorldPath(root)); os.IsNotExist(err) {
		return nil
	}
	lock, err := pkgdb.AcquireLock(root, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	set, err := LoadList(WorldPath(root))
	if err != nil {
		return err
	}
	changed := false
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if set[p] {
			delete(set, p)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return SaveList(WorldPath(root), set)
}

// LoadHold returns the merged set of held packages from configHold (kpm.ini [repo] hold)
// and <root>/var/lib/kpm/hold.
func LoadHold(root string, configHold []string) (map[string]bool, error) {
	if lock, err := pkgdb.AcquireLock(root, false); err == nil {
		defer lock.Release()
	}
	set, err := LoadList(HoldPath(root))
	if err != nil {
		return nil, err
	}
	for _, p := range configHold {
		p = strings.TrimSpace(p)
		if p != "" {
			set[p] = true
		}
	}
	return set, nil
}

// AddHold adds package names to <root>/var/lib/kpm/hold under an exclusive lock.
func AddHold(root string, pkgs ...string) error {
	if len(pkgs) == 0 {
		return nil
	}
	_ = pkgdb.EnsureInitialized(root)
	lock, err := pkgdb.AcquireLock(root, true)
	if err != nil {
		return err
	}
	defer lock.Release()

	set, err := LoadList(HoldPath(root))
	if err != nil {
		return err
	}
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if p != "" {
			set[p] = true
		}
	}
	return SaveList(HoldPath(root), set)
}

// RemoveHold removes package names from <root>/var/lib/kpm/hold under an exclusive lock.
func RemoveHold(root string, pkgs ...string) (removed []string, err error) {
	if len(pkgs) == 0 {
		return nil, nil
	}
	_ = pkgdb.EnsureInitialized(root)
	lock, err := pkgdb.AcquireLock(root, true)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	set, err := LoadList(HoldPath(root))
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if set[p] {
			delete(set, p)
			removed = append(removed, p)
		}
	}
	if len(removed) > 0 {
		if err := SaveList(HoldPath(root), set); err != nil {
			return nil, err
		}
	}
	return removed, nil
}

// LoadList reads a newline-delimited package list file. Missing files return an empty set.
func LoadList(path string) (map[string]bool, error) {
	set := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return set, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		set[line] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return set, nil
}

// SaveList atomically writes a sorted package list file.
func SaveList(path string, set map[string]bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(set))
	for k := range set {
		k = strings.TrimSpace(k)
		if k != "" {
			names = append(names, k)
		}
	}
	sort.Strings(names)

	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(n)
		sb.WriteByte('\n')
	}

	tmp, err := os.CreateTemp(dir, ".kpm-state-*")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(sb.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func normalizeRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/"
	}
	return root
}
