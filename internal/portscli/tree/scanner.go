// Package ports implements pure-Go CRUX ports tree scanning, Pkgfile metadata
// parsing, local search, and kpm-compatible version diffing with zero shell-outs.
package tree

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Port holds metadata parsed natively from a CRUX Pkgfile.
type Port struct {
	Name        string
	Version     string
	Release     string
	Collection  string
	RelDir      string
	Dir         string
	Description string
	URL         string
	Maintainer  string
	Packager    string
	Depends     []string
	MakeDepends []string
}

// FullVersion returns "<version>-<release>" (or "<version>" if release is empty),
// matching kpm/internal/ports.Port.FullVersion().
func (p Port) FullVersion() string {
	if p.Release == "" {
		return p.Version
	}
	return p.Version + "-" + p.Release
}

type dirID struct {
	dev uint64
	ino uint64
}

// walkPkgfiles recursively finds all directories containing a Pkgfile under searchRoot,
// following symlinks with (dev, ino) cycle protection, matching kpm/internal/ports.
func walkPkgfiles(portsDir, searchRoot, prefix string, seen map[string]bool, out *[]string) error {
	visited := make(map[dirID]bool)

	var walk func(cur string) error
	walk = func(cur string) error {
		info, err := os.Stat(cur)
		if err != nil || !info.IsDir() {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			id := dirID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
			if visited[id] {
				return nil
			}
			visited[id] = true
		}

		entries, err := os.ReadDir(cur)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(cur, name)
			if name == "Pkgfile" {
				if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
					rel, err := filepath.Rel(portsDir, cur)
					if err == nil {
						rel = filepath.ToSlash(rel)
						if rel == "." {
							rel = filepath.Base(portsDir)
						}
						if prefix != "" && !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
							rel = prefix + "/" + rel
						}
						if !seen[rel] {
							seen[rel] = true
							*out = append(*out, rel)
						}
					}
				}
				continue
			}
			if strings.HasPrefix(name, ".") {
				continue
			}
			fi, err := os.Stat(full)
			if err != nil || !fi.IsDir() {
				continue
			}
			_ = walk(full)
		}
		return nil
	}

	return walk(searchRoot)
}

// ListEntries discovers all Pkgfiles under portsDir, returning sorted relative
// port paths (e.g. "core/zlib", "opt/git") matching CRUX `ports -l` and `kpm list --ports`.
func ListEntries(portsDir string, collections []string) ([]string, error) {
	info, err := os.Stat(portsDir)
	if err != nil {
		return nil, fmt.Errorf("ports directory %q not found: %w", portsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ports path %q is not a directory", portsDir)
	}

	var entries []string
	seen := make(map[string]bool)

	if len(collections) > 0 {
		foundAny := false
		for _, col := range collections {
			colDir := filepath.Join(portsDir, col)
			if ci, err := os.Stat(colDir); err == nil && ci.IsDir() {
				foundAny = true
				_ = walkPkgfiles(portsDir, colDir, col, seen, &entries)
			}
		}
		if !foundAny {
			_ = walkPkgfiles(portsDir, portsDir, "", seen, &entries)
		}
	} else {
		_ = walkPkgfiles(portsDir, portsDir, "", seen, &entries)
	}

	sort.Strings(entries)
	return entries, nil
}

// DiscoverAll parses metadata for all discovered ports under portsDir natively in Go.
func DiscoverAll(portsDir string, collections []string) ([]Port, error) {
	entries, err := ListEntries(portsDir, collections)
	if err != nil {
		return nil, err
	}
	out := make([]Port, 0, len(entries))
	for _, rel := range entries {
		portDir := filepath.Join(portsDir, filepath.FromSlash(rel))
		p, err := ParsePkgfile(portDir, portsDir)
		if err != nil {
			continue
		}
		p.RelDir = rel
		out = append(out, p)
	}
	return out, nil
}

// PriorityMap collapses discovered ports into a map keyed by package name,
// honoring kpm.ini's collection priority order (first collection wins).
func PriorityMap(all []Port, collections []string) map[string]Port {
	rank := make(map[string]int, len(collections))
	for i, c := range collections {
		rank[c] = i
	}
	rankOf := func(col string) int {
		if r, ok := rank[col]; ok {
			return r
		}
		return len(rank) + 1
	}

	result := make(map[string]Port, len(all))
	for _, p := range all {
		if p.Name == "" {
			continue
		}
		if prev, ok := result[p.Name]; !ok || rankOf(p.Collection) < rankOf(prev.Collection) {
			result[p.Name] = p
		}
	}
	return result
}
