// Package srcmachine implements kryx-src, a unified xbps-src-style source build,
// linting, and release-staging engine sharing pe's internal packages.
package srcmachine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kryx-linux/pe/internal/ports"
)

// Entry represents a discovered port in a canonical registry, preserving both
// its filesystem path and its declared Pkgfile metadata.
type Entry struct {
	Port   ports.Port
	RelDir string // Slash-normalized relative path under PortsDir (e.g. "rolling/linux-6.18" or "." for root port)
	Base   string // Directory basename (e.g. "linux-6.18")
}

// Registry is an in-memory index of all Pkgfile ports under a ports tree
// (supporting both multi-collection trees and single-port repositories).
type Registry struct {
	PortsDir       string
	Collections    []string
	IsSinglePort   bool
	SinglePort     *Entry
	Entries        []Entry
	byRelDir       map[string]Entry
	byQualName     map[string]Entry
	byPkgName      map[string][]Entry
	byDirBase      map[string][]Entry
	collectionRank map[string]int
}

// LoadRegistry scans portsDir once and builds a canonical lookup registry.
// If <portsDir>/Pkgfile exists directly at the root, the repository is treated
// as a single-port repository without needing to copy it into a temp directory.
func LoadRegistry(portsDir string, collections []string) (*Registry, error) {
	absPorts, err := filepath.Abs(portsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve ports dir %q: %w", portsDir, err)
	}
	info, err := os.Stat(absPorts)
	if err != nil {
		return nil, fmt.Errorf("ports directory %q not found: %w", portsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ports path %q is not a directory", portsDir)
	}

	reg := &Registry{
		PortsDir:       absPorts,
		Collections:    append([]string(nil), collections...),
		byRelDir:       make(map[string]Entry),
		byQualName:     make(map[string]Entry),
		byPkgName:      make(map[string][]Entry),
		byDirBase:      make(map[string][]Entry),
		collectionRank: make(map[string]int, len(collections)),
	}
	for i, c := range collections {
		reg.collectionRank[c] = i
	}

	// Single-port repository (Pkgfile directly at repo root).
	rootPkgfile := filepath.Join(absPorts, "Pkgfile")
	if fi, statErr := os.Stat(rootPkgfile); statErr == nil && !fi.IsDir() {
		p, parseErr := ports.ParsePkgfileFast(absPorts, filepath.Dir(absPorts))
		if parseErr != nil {
			return nil, fmt.Errorf("parse root Pkgfile in %s: %w", absPorts, parseErr)
		}
		p.Dir = absPorts
		p.RelDir = p.Name
		p.Collection = ""
		e := Entry{
			Port:   p,
			RelDir: p.Name,
			Base:   filepath.Base(absPorts),
		}
		reg.IsSinglePort = true
		reg.SinglePort = &e
		reg.addEntry(e)
		return reg, nil
	}

	relDirs, err := ports.ListEntries(absPorts, collections)
	if err != nil {
		return nil, err
	}

	for _, rel := range relDirs {
		portDir := filepath.Join(absPorts, filepath.FromSlash(rel))
		p, parseErr := ports.ParsePkgfileFast(portDir, absPorts)
		if parseErr != nil {
			continue
		}
		p.RelDir = rel
		e := Entry{
			Port:   p,
			RelDir: rel,
			Base:   filepath.Base(portDir),
		}
		reg.addEntry(e)
	}

	// Sort candidate slices by collection priority, then shallower depth, then RelDir.
	sortCandidates := func(list []Entry) {
		sort.Slice(list, func(i, j int) bool {
			ri, rj := reg.rank(list[i].Port.Collection), reg.rank(list[j].Port.Collection)
			if ri != rj {
				return ri < rj
			}
			di, dj := strings.Count(list[i].RelDir, "/"), strings.Count(list[j].RelDir, "/")
			if di != dj {
				return di < dj
			}
			return list[i].RelDir < list[j].RelDir
		})
	}
	for k := range reg.byPkgName {
		sortCandidates(reg.byPkgName[k])
	}
	for k := range reg.byDirBase {
		sortCandidates(reg.byDirBase[k])
	}

	return reg, nil
}

func (r *Registry) addEntry(e Entry) {
	r.Entries = append(r.Entries, e)
	r.byRelDir[e.RelDir] = e
	qual := e.Port.QualifiedName()
	if prev, exists := r.byQualName[qual]; !exists || r.rank(e.Port.Collection) < r.rank(prev.Port.Collection) {
		r.byQualName[qual] = e
	}
	if e.Port.Name != "" {
		r.byPkgName[e.Port.Name] = append(r.byPkgName[e.Port.Name], e)
	}
	if e.Base != "" {
		r.byDirBase[e.Base] = append(r.byDirBase[e.Base], e)
	}
}

func (r *Registry) rank(col string) int {
	if rk, ok := r.collectionRank[col]; ok {
		return rk
	}
	return len(r.collectionRank) + 1
}

// Resolve locates a single port entry by:
//  1. Exact relative directory path (e.g. "rolling/linux-6.18")
//  2. Qualified collection/name (e.g. "rolling/kernel")
//  3. Declared Pkgfile name (e.g. "kernel") in collection priority order
//  4. Directory basename (e.g. "linux-6.18") in collection priority order
//  5. Trailing path suffix (e.g. "dev/gports")
func (r *Registry) Resolve(target string) (Entry, bool) {
	norm := normalizeTarget(target)
	if norm == "" {
		return Entry{}, false
	}
	if r.IsSinglePort && r.SinglePort != nil {
		if norm == "." || norm == "all" || norm == r.SinglePort.Port.Name || norm == r.SinglePort.Base {
			return *r.SinglePort, true
		}
		return Entry{}, false
	}

	if e, ok := r.byRelDir[norm]; ok {
		return e, true
	}
	if e, ok := r.byQualName[norm]; ok {
		return e, true
	}
	if cands := r.byPkgName[norm]; len(cands) > 0 {
		return cands[0], true
	}
	if cands := r.byDirBase[norm]; len(cands) > 0 {
		return cands[0], true
	}
	// Suffix match on RelDir (e.g. "dev/gports" matches "extra/dev/gports")
	suffix := "/" + norm
	for _, e := range r.Entries {
		if strings.HasSuffix(e.RelDir, suffix) {
			return e, true
		}
	}
	return Entry{}, false
}

// ExpandTargets resolves a list of user/CI target specs into unique canonical Entries.
// Supports:
//   - "all" (every discovered port)
//   - Single-port repo aliases (".", repo basename, package name, "all")
//   - Individual ports (by relative dir, dir basename, Pkgfile name, or collection/name)
//   - Collection/directory prefixes (e.g. "rolling" expands to all ports under rolling/)
func (r *Registry) ExpandTargets(targets []string, repoHint string) ([]Entry, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no target ports specified")
	}

	if r.IsSinglePort && r.SinglePort != nil {
		repoBase := ""
		if repoHint != "" {
			parts := strings.Split(strings.TrimSuffix(repoHint, ".git"), "/")
			repoBase = parts[len(parts)-1]
		}
		for _, raw := range targets {
			norm := normalizeTarget(raw)
			if norm == "" || norm == "." || norm == "all" ||
				norm == r.SinglePort.Port.Name || norm == r.SinglePort.Base ||
				(repoBase != "" && norm == repoBase) {
				continue
			}
			return nil, fmt.Errorf("single-port repo defines %q at root, not %q", r.SinglePort.Port.Name, raw)
		}
		return []Entry{*r.SinglePort}, nil
	}

	var out []Entry
	seen := make(map[string]bool)
	add := func(e Entry) {
		if !seen[e.Port.Dir] {
			seen[e.Port.Dir] = true
			out = append(out, e)
		}
	}

	for _, raw := range targets {
		norm := normalizeTarget(raw)
		if norm == "" {
			continue
		}
		if norm == "all" {
			for _, e := range r.Entries {
				add(e)
			}
			continue
		}
		if e, ok := r.Resolve(norm); ok {
			add(e)
			continue
		}
		// Check if norm is a collection / directory prefix containing nested ports.
		prefix := norm + "/"
		matchedGroup := 0
		for _, e := range r.Entries {
			if strings.HasPrefix(e.RelDir, prefix) {
				add(e)
				matchedGroup++
			}
		}
		if matchedGroup > 0 {
			continue
		}
		return nil, fmt.Errorf("port or collection %q not found in %s", raw, r.PortsDir)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no ports matched targets %v in %s", targets, r.PortsDir)
	}
	return out, nil
}

// ResolveDepsOrder computes a deterministic topological (dependencies-first) build
// order for the given root entries using the canonical Registry, evaluating each
// selected Pkgfile fully via bash so source/renames/version are exact.
func (r *Registry) ResolveDepsOrder(roots []Entry, includeDeps bool) (ordered []Entry, missing []string, err error) {
	if !includeDeps {
		res := make([]Entry, 0, len(roots))
		for _, root := range roots {
			full, perr := ports.ParsePkgfile(root.Port.Dir, r.PortsDir)
			if perr != nil {
				return nil, nil, perr
			}
			full.RelDir = root.RelDir
			if r.IsSinglePort {
				full.Collection = ""
			}
			root.Port = full
			res = append(res, root)
		}
		return res, nil, nil
	}

	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	missingSet := make(map[string]bool)

	var dfs func(e Entry) error
	dfs = func(e Entry) error {
		key := e.Port.Dir
		if visited[key] || visiting[key] {
			return nil
		}
		visiting[key] = true
		for _, dep := range e.Port.Depends {
			depEntry, ok := r.Resolve(dep)
			if !ok {
				if !missingSet[dep] {
					missingSet[dep] = true
					missing = append(missing, dep)
				}
				continue
			}
			if err := dfs(depEntry); err != nil {
				return err
			}
		}
		delete(visiting, key)
		if !visited[key] {
			visited[key] = true
			full, perr := ports.ParsePkgfile(e.Port.Dir, r.PortsDir)
			if perr != nil {
				return perr
			}
			full.RelDir = e.RelDir
			if r.IsSinglePort {
				full.Collection = ""
			}
			e.Port = full
			ordered = append(ordered, e)
		}
		return nil
	}

	for _, root := range roots {
		if err := dfs(root); err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(missing)
	return ordered, missing, nil
}

func normalizeTarget(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimSuffix(s, "/")
	return filepath.ToSlash(s)
}
