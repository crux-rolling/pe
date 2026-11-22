// Package ports discovers, parses, and optionally syncs CRUX Linux ports trees
// in place without relocating port directories.
package ports

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Port holds parsed metadata from a CRUX Pkgfile.
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
	Source      []string
	Renames     []string
}

// AllBuildDepends returns deduplicated runtime Depends followed by build-only
// MakeDepends in declaration order. Ports without MakeDepends return Depends unchanged.
func (p Port) AllBuildDepends() []string {
	if len(p.MakeDepends) == 0 {
		return p.Depends
	}
	out := make([]string, 0, len(p.Depends)+len(p.MakeDepends))
	seen := make(map[string]bool, len(p.Depends)+len(p.MakeDepends))
	for _, d := range p.Depends {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, d := range p.MakeDepends {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// FullVersion returns "<version>-<release>" (or "<version>" if release is empty).
func (p Port) FullVersion() string {
	if p.Release == "" {
		return p.Version
	}
	return p.Version + "-" + p.Release
}

// QualifiedName returns "<collection>/<name>" or "<name>" if collection is empty.
func (p Port) QualifiedName() string {
	if p.Collection == "" {
		return p.Name
	}
	return p.Collection + "/" + p.Name
}

// PortPath returns the port's relative directory path under ports_dir when known,
// falling back to QualifiedName().
func (p Port) PortPath() string {
	if p.RelDir != "" {
		return p.RelDir
	}
	return p.QualifiedName()
}

type dirID struct {
	dev uint64
	ino uint64
}

// walkPkgfiles recursively finds all directories containing a Pkgfile under searchRoot,
// following symlinks (matching CRUX `find -follow`) with cycle protection.
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
			// Follow symlinks to directories via os.Stat.
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

// ListEntries discovers all Pkgfiles under portsDir recursively in place (following symlinks),
// returning sorted relative port paths matching CRUX `ports -l` (e.g. "core/zlib", "opt/git").
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
		foundAnyCollectionDir := false
		for _, col := range collections {
			colDir := filepath.Join(portsDir, col)
			if ci, err := os.Stat(colDir); err == nil && ci.IsDir() {
				foundAnyCollectionDir = true
				_ = walkPkgfiles(portsDir, colDir, col, seen, &entries)
			}
		}
		if !foundAnyCollectionDir {
			_ = walkPkgfiles(portsDir, portsDir, "", seen, &entries)
		}
	} else {
		_ = walkPkgfiles(portsDir, portsDir, "", seen, &entries)
	}

	sort.Strings(entries)
	return entries, nil
}

// DiscoverAll parses metadata for all ports under portsDir.
// Uses fast Go parsing with bash fallback for dynamic Pkgfiles.
func DiscoverAll(portsDir string, collections []string) ([]Port, error) {
	entries, err := ListEntries(portsDir, collections)
	if err != nil {
		return nil, err
	}
	result := make([]Port, 0, len(entries))
	for _, rel := range entries {
		portDir := filepath.Join(portsDir, filepath.FromSlash(rel))
		if _, err := os.Stat(filepath.Join(portDir, "Pkgfile")); err != nil {
			continue
		}
		p, err := ParsePkgfileFast(portDir, portsDir)
		if err != nil {
			continue
		}
		result = append(result, p)
	}
	return result, nil
}

// Find locates a single port by name or relative path, honoring collections priority order
// and falling back to a full recursive search (following symlinks) under portsDir.
func Find(portsDir string, collections []string, target string) (Port, error) {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, "/")
	if target == "" {
		return Port{}, fmt.Errorf("empty port name")
	}

	// Explicit path under portsDir (e.g. "core/zlib" or "extra/dev/gports")
	if strings.Contains(target, "/") {
		dir := filepath.Join(portsDir, filepath.FromSlash(target))
		if _, err := os.Stat(filepath.Join(dir, "Pkgfile")); err == nil {
			return ParsePkgfile(dir, portsDir)
		}
		// Also accept a direct existing filesystem path to a port dir or Pkgfile.
		directDir := target
		if filepath.Base(directDir) == "Pkgfile" {
			directDir = filepath.Dir(directDir)
		}
		if _, err := os.Stat(filepath.Join(directDir, "Pkgfile")); err == nil {
			return ParsePkgfile(directDir, portsDir)
		}
	}

	// Priority order by configured collections (direct and nested within each collection)
	for _, col := range collections {
		dir := filepath.Join(portsDir, col, target)
		if _, err := os.Stat(filepath.Join(dir, "Pkgfile")); err == nil {
			return ParsePkgfile(dir, portsDir)
		}
		colRoot := filepath.Join(portsDir, col)
		if matched, ok := findRecursiveMatch(portsDir, colRoot, col, target); ok {
			return ParsePkgfile(matched, portsDir)
		}
	}

	// Direct flat layout <portsDir>/<target>/Pkgfile
	direct := filepath.Join(portsDir, target)
	if _, err := os.Stat(filepath.Join(direct, "Pkgfile")); err == nil {
		return ParsePkgfile(direct, portsDir)
	}

	// Full recursive scan of portsDir at arbitrary depth (following symlinks)
	if matched, ok := findRecursiveMatch(portsDir, portsDir, "", target); ok {
		return ParsePkgfile(matched, portsDir)
	}

	// Fallback: match by declared Pkgfile `name=` (handling ports whose directory
	// name differs from their package name, e.g. rolling/linux-6.18 declaring name=kernel).
	if matched, ok := findByPkgfileName(portsDir, collections, target); ok {
		return ParsePkgfile(matched, portsDir)
	}

	return Port{}, fmt.Errorf("port %q not found in %s", target, portsDir)
}

func findByPkgfileName(portsDir string, collections []string, target string) (string, bool) {
	normTarget := filepath.ToSlash(strings.Trim(target, "/"))
	wantCol := ""
	wantName := normTarget
	if slash := strings.LastIndex(normTarget, "/"); slash >= 0 {
		wantCol = normTarget[:slash]
		wantName = normTarget[slash+1:]
	}
	if wantName == "" {
		return "", false
	}

	// Build priority-ordered search roots (configured collections first, then full tree).
	var entries []string
	seen := make(map[string]bool)
	if wantCol != "" {
		colDir := filepath.Join(portsDir, filepath.FromSlash(wantCol))
		if fi, err := os.Stat(colDir); err == nil && fi.IsDir() {
			_ = walkPkgfiles(portsDir, colDir, wantCol, seen, &entries)
		}
	} else {
		for _, col := range collections {
			colDir := filepath.Join(portsDir, col)
			if fi, err := os.Stat(colDir); err == nil && fi.IsDir() {
				_ = walkPkgfiles(portsDir, colDir, col, seen, &entries)
			}
		}
		_ = walkPkgfiles(portsDir, portsDir, "", seen, &entries)
	}

	for _, rel := range entries {
		dir := filepath.Join(portsDir, filepath.FromSlash(rel))
		p, err := ParsePkgfileFast(dir, portsDir)
		if err != nil {
			continue
		}
		if p.Name != wantName {
			continue
		}
		if wantCol != "" && p.Collection != wantCol {
			continue
		}
		return dir, true
	}
	return "", false
}

// FindGroup expands target into all nested ports when target is a collection/directory
// under portsDir containing one or more Pkgfiles (e.g. `pe build gports` where gports/
// holds multiple sub-ports). Returns nil, nil if target is not a directory with nested ports.
func FindGroup(portsDir, target string) ([]Port, error) {
	target = strings.TrimSpace(target)
	target = strings.TrimSuffix(target, "/")
	if target == "" {
		return nil, nil
	}
	groupDir := filepath.Join(portsDir, filepath.FromSlash(target))
	info, err := os.Stat(groupDir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var entries []string
	seen := make(map[string]bool)
	_ = walkPkgfiles(portsDir, groupDir, "", seen, &entries)
	if len(entries) == 0 {
		return nil, nil
	}
	sort.Strings(entries)
	result := make([]Port, 0, len(entries))
	for _, rel := range entries {
		dir := filepath.Join(portsDir, filepath.FromSlash(rel))
		p, err := ParsePkgfile(dir, portsDir)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

func findRecursiveMatch(portsDir, searchRoot, prefix, target string) (string, bool) {
	var entries []string
	seen := make(map[string]bool)
	_ = walkPkgfiles(portsDir, searchRoot, prefix, seen, &entries)
	if len(entries) == 0 {
		return "", false
	}
	// Prefer exact relative match, then basename/suffix match with shortest depth first.
	sort.Slice(entries, func(i, j int) bool {
		di := strings.Count(entries[i], "/")
		dj := strings.Count(entries[j], "/")
		if di != dj {
			return di < dj
		}
		return entries[i] < entries[j]
	})
	normTarget := filepath.ToSlash(target)
	for _, rel := range entries {
		if rel == normTarget || strings.HasSuffix(rel, "/"+normTarget) {
			return filepath.Join(portsDir, filepath.FromSlash(rel)), true
		}
	}
	return "", false
}

// ParsePkgfile parses header comments and evaluates name/version/release/source/renames
// using bash, matching CRUX pkgmk's exact Pkgfile semantics.
func ParsePkgfile(portDir, portsDir string) (Port, error) {
	pkgfile := filepath.Join(portDir, "Pkgfile")
	p, err := parseHeaders(pkgfile)
	if err != nil {
		return Port{}, err
	}
	p.Dir = portDir
	p.RelDir = inferRelDir(portDir, portsDir)
	p.Collection = inferCollection(portDir, portsDir)

	if err := evalPkgfileBash(&p, portDir); err != nil {
		return Port{}, err
	}
	return p, nil
}

// ParsePkgfileFast parses headers and simple variable assignments directly in Go,
// falling back to bash only if version/name/release contain shell command substitutions.
func ParsePkgfileFast(portDir, portsDir string) (Port, error) {
	pkgfile := filepath.Join(portDir, "Pkgfile")
	p, err := parseHeaders(pkgfile)
	if err != nil {
		return Port{}, err
	}
	p.Dir = portDir
	p.RelDir = inferRelDir(portDir, portsDir)
	p.Collection = inferCollection(portDir, portsDir)

	data, err := os.ReadFile(pkgfile)
	if err != nil {
		return Port{}, err
	}

	needsBash := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "build()") || strings.HasPrefix(line, "build ()") {
			break
		}
		for _, field := range []struct {
			prefix string
			target *string
		}{
			{"name=", &p.Name},
			{"version=", &p.Version},
			{"release=", &p.Release},
		} {
			if strings.HasPrefix(line, field.prefix) {
				v := strings.TrimSpace(strings.TrimPrefix(line, field.prefix))
				v = trimShellQuotes(v)
				if strings.Contains(v, "$(") || strings.Contains(v, "`") || strings.Contains(v, "$") {
					needsBash = true
				} else {
					*field.target = v
				}
			}
		}
	}

	if needsBash || p.Name == "" || p.Version == "" || p.Release == "" {
		if err := evalPkgfileBash(&p, portDir); err != nil {
			if p.Name == "" {
				p.Name = filepath.Base(portDir)
			}
			return p, nil
		}
	}
	return p, nil
}

// ResolveDeps resolves recursive Pkgfile dependencies in post-order (dependencies first).
func ResolveDeps(portsDir string, collections []string, targets []string) (ordered []Port, missing []string, err error) {
	roots := make([]Port, 0, len(targets))
	for _, t := range targets {
		p, findErr := Find(portsDir, collections, t)
		if findErr != nil {
			return nil, nil, findErr
		}
		roots = append(roots, p)
	}
	return ResolveDepsForPorts(portsDir, collections, roots)
}

// ResolveDepsForPorts resolves recursive Pkgfile dependencies for already-discovered
// root ports in post-order (dependencies first), avoiding lossy re-lookups when a
// port's directory name differs from its Pkgfile `name=`.
func ResolveDepsForPorts(portsDir string, collections []string, roots []Port) (ordered []Port, missing []string, err error) {
	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	missingSet := make(map[string]bool)

	var visitPort func(p Port) error
	var dfsName func(name string) error

	visitPort = func(p Port) error {
		key := p.Name
		if key == "" {
			key = p.Dir
		}
		if visited[key] || visiting[key] {
			return nil
		}
		visiting[key] = true
		for _, dep := range p.AllBuildDepends() {
			if err := dfsName(dep); err != nil {
				return err
			}
		}
		delete(visiting, key)
		if !visited[key] {
			visited[key] = true
			ordered = append(ordered, p)
		}
		return nil
	}

	dfsName = func(name string) error {
		if visited[name] || visiting[name] {
			return nil
		}
		p, findErr := Find(portsDir, collections, name)
		if findErr != nil {
			if !missingSet[name] {
				missingSet[name] = true
				missing = append(missing, name)
			}
			return nil
		}
		return visitPort(p)
	}

	for _, r := range roots {
		if err := visitPort(r); err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(missing)
	return ordered, missing, nil
}

// BinaryDepLookup reports whether a prebuilt binary is available for pkgName
// and returns its runtime dependencies (if recorded in the binary index).
type BinaryDepLookup func(pkgName string) (runtimeDeps []string, ok bool)

// PortLookup resolves a package/port name from a local ports tree.
type PortLookup func(name string) (Port, error)

// BuildStep is one ordered step in a binary-aware dependency build plan.
type BuildStep struct {
	Name             string
	Port             *Port // Non-nil for local source builds (and optional metadata for binary steps)
	IsTarget         bool  // Explicitly requested root target (always compiled from source)
	FromBinary       bool  // Install prebuilt binary from index.json instead of compiling from source
	AlreadyInstalled bool  // Dependency is already present in <root>/var/lib/pkg/db (skipped without expanding its deps)
}

// ResolveBuildPlan computes a topological (dependencies-first) build plan for roots:
//   - Explicitly requested root targets always build from source and require both
//     Depends and MakeDepends.
//   - Already-installed dependencies are skipped without expanding their deps.
//   - When lookupBinary is non-nil and finds a prebuilt binary for an uninstalled
//     dependency, that dependency is scheduled as FromBinary and only its runtime
//     Depends are expanded (its MakeDepends are NOT needed). This also succeeds for
//     binary-only dependencies absent from the local ports tree.
//   - Dependencies that must be compiled from source expand both Depends and MakeDepends.
func ResolveBuildPlan(roots []Port, findPort PortLookup, installed map[string]bool, lookupBinary BinaryDepLookup) (plan []BuildStep, missing []string, err error) {
	rootByName := make(map[string]Port, len(roots))
	for _, r := range roots {
		if r.Name != "" {
			rootByName[r.Name] = r
		}
	}

	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	missingSet := make(map[string]bool)

	var visitSource func(p Port, isTarget bool) error
	var visitDep func(name string) error

	visitSource = func(p Port, isTarget bool) error {
		key := p.Name
		if key == "" {
			key = p.Dir
		}
		if visited[key] || visiting[key] {
			return nil
		}
		visiting[key] = true
		for _, dep := range p.AllBuildDepends() {
			if err := visitDep(dep); err != nil {
				return err
			}
		}
		delete(visiting, key)
		if !visited[key] {
			visited[key] = true
			pc := p
			plan = append(plan, BuildStep{
				Name:       p.Name,
				Port:       &pc,
				IsTarget:   isTarget,
				FromBinary: false,
			})
		}
		return nil
	}

	visitDep = func(name string) error {
		if name == "" || visited[name] || visiting[name] {
			return nil
		}
		// If a dependency is itself one of the requested root targets, build it first.
		if rootPort, isRoot := rootByName[name]; isRoot {
			return visitSource(rootPort, true)
		}
		if installed[name] {
			visited[name] = true
			plan = append(plan, BuildStep{
				Name:             name,
				AlreadyInstalled: true,
			})
			return nil
		}
		visiting[name] = true

		// Binary-first dependency resolution: only runtime Depends are required.
		if lookupBinary != nil {
			if binDeps, ok := lookupBinary(name); ok {
				deps := binDeps
				var localPort *Port
				if findPort != nil {
					if p, perr := findPort(name); perr == nil {
						pc := p
						localPort = &pc
						if len(deps) == 0 {
							deps = p.Depends
						}
					}
				}
				for _, d := range deps {
					if err := visitDep(d); err != nil {
						return err
					}
				}
				delete(visiting, name)
				if !visited[name] {
					visited[name] = true
					plan = append(plan, BuildStep{
						Name:       name,
						Port:       localPort,
						IsTarget:   false,
						FromBinary: true,
					})
				}
				return nil
			}
		}

		// No binary available (or --source-deps): resolve from local ports tree and build from source.
		if findPort == nil {
			delete(visiting, name)
			if !missingSet[name] {
				missingSet[name] = true
				missing = append(missing, name)
			}
			return nil
		}
		p, findErr := findPort(name)
		if findErr != nil {
			delete(visiting, name)
			if !missingSet[name] {
				missingSet[name] = true
				missing = append(missing, name)
			}
			return nil
		}
		delete(visiting, name)
		return visitSource(p, false)
	}

	for _, r := range roots {
		if err := visitSource(r, true); err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(missing)
	return plan, missing, nil
}

// SyncPorts runs CRUX port collection drivers under driversDir matching config files
// in confDir (mirroring CRUX `ports -u`).
func SyncPorts(portsDir, confDir, driversDir string, collections []string, out, errOut io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	if _, err := os.Stat(portsDir); err != nil {
		return fmt.Errorf("ports directory %q not found: %w", portsDir, err)
	}
	entries, err := os.ReadDir(driversDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no port drivers found in %s", driversDir)
		}
		return fmt.Errorf("read port drivers dir %s: %w", driversDir, err)
	}

	var drivers []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(driversDir, e.Name())
		info, err := os.Stat(full)
		if err == nil && info.Mode()&0o111 != 0 {
			drivers = append(drivers, full)
		}
	}
	if len(drivers) == 0 {
		return fmt.Errorf("no executable port drivers found in %s", driversDir)
	}
	sort.Strings(drivers)

	runDriver := func(driver, confFile string) error {
		cmd := exec.Command(driver, confFile)
		cmd.Env = append(os.Environ(), "PORTS_DIR="+portsDir)
		cmd.Stdout = out
		cmd.Stderr = errOut
		return cmd.Run()
	}

	ran := 0
	if len(collections) > 0 {
		for _, col := range collections {
			matched := false
			for _, driver := range drivers {
				suffix := filepath.Base(driver)
				confFile := filepath.Join(confDir, col+"."+suffix)
				if info, err := os.Stat(confFile); err == nil && !info.IsDir() {
					matched = true
					ran++
					if err := runDriver(driver, confFile); err != nil {
						return fmt.Errorf("sync collection %q via %s: %w", col, suffix, err)
					}
				}
			}
			if !matched {
				fmt.Fprintf(errOut, "pe: ports collection %q not found in %s\n", col, confDir)
			}
		}
	} else {
		for _, driver := range drivers {
			suffix := filepath.Base(driver)
			matches, _ := filepath.Glob(filepath.Join(confDir, "*."+suffix))
			sort.Strings(matches)
			for _, confFile := range matches {
				ran++
				if err := runDriver(driver, confFile); err != nil {
					return fmt.Errorf("sync %s via %s: %w", filepath.Base(confFile), suffix, err)
				}
			}
		}
	}

	if ran == 0 {
		return fmt.Errorf("no matching port collection files found in %s", confDir)
	}
	return nil
}

func parseHeaders(pkgfile string) (Port, error) {
	f, err := os.Open(pkgfile)
	if err != nil {
		return Port{}, fmt.Errorf("open %s: %w", pkgfile, err)
	}
	defer f.Close()

	var p Port
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		colon := strings.Index(body, ":")
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(body[:colon]))
		val := strings.TrimSpace(body[colon+1:])
		switch key {
		case "description":
			p.Description = val
		case "url":
			p.URL = val
		case "maintainer":
			p.Maintainer = val
		case "packager":
			p.Packager = val
		case "depends on", "depends":
			p.Depends = appendUnique(p.Depends, parseDepends(val)...)
		case "make depends on", "make depends", "makedepends", "build depends on", "build depends":
			p.MakeDepends = appendUnique(p.MakeDepends, parseDepends(val)...)
		}
	}
	if err := scanner.Err(); err != nil {
		return Port{}, err
	}
	return p, nil
}

func appendUnique(dst []string, items ...string) []string {
	seen := make(map[string]bool, len(dst)+len(items))
	for _, s := range dst {
		seen[s] = true
	}
	for _, item := range items {
		if item != "" && !seen[item] {
			seen[item] = true
			dst = append(dst, item)
		}
	}
	return dst
}

func parseDepends(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	var deps []string
	seen := make(map[string]bool)
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" && !seen[f] {
			seen[f] = true
			deps = append(deps, f)
		}
	}
	return deps
}

func evalPkgfileBash(p *Port, portDir string) error {
	script := `
set -e
source ./Pkgfile >/dev/null
printf 'NAME\t%s\0' "${name:-}"
printf 'VERSION\t%s\0' "${version:-}"
printf 'RELEASE\t%s\0' "${release:-}"
if [ "$(type -t build)" = "function" ]; then
	printf 'HAS_BUILD\t1\0'
else
	printf 'HAS_BUILD\t0\0'
fi
for s in "${source[@]}"; do
	printf 'SOURCE\t%s\0' "$s"
done
for r in "${renames[@]}"; do
	printf 'RENAME\t%s\0' "$r"
done
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = portDir
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("evaluate %s/Pkgfile: %w: %s", portDir, err, strings.TrimSpace(stderr.String()))
	}

	p.Source = nil
	p.Renames = nil
	hasBuild := false
	records := bytes.Split(stdout.Bytes(), []byte{0})
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		parts := bytes.SplitN(rec, []byte{'\t'}, 2)
		if len(parts) != 2 {
			continue
		}
		key := string(parts[0])
		val := string(parts[1])
		switch key {
		case "NAME":
			p.Name = val
		case "VERSION":
			p.Version = val
		case "RELEASE":
			p.Release = val
		case "HAS_BUILD":
			hasBuild = val == "1"
		case "SOURCE":
			p.Source = append(p.Source, val)
		case "RENAME":
			p.Renames = append(p.Renames, val)
		}
	}
	if p.Name == "" {
		return fmt.Errorf("%s/Pkgfile: variable 'name' not specified", portDir)
	}
	if p.Version == "" {
		return fmt.Errorf("%s/Pkgfile: variable 'version' not specified", portDir)
	}
	if p.Release == "" {
		return fmt.Errorf("%s/Pkgfile: variable 'release' not specified", portDir)
	}
	if !hasBuild {
		return fmt.Errorf("%s/Pkgfile: function 'build' not specified", portDir)
	}
	return nil
}

func inferRelDir(portDir, portsDir string) string {
	if portsDir == "" {
		return ""
	}
	rel, err := filepath.Rel(portsDir, portDir)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}

func inferCollection(portDir, portsDir string) string {
	if portsDir == "" {
		return ""
	}
	rel, err := filepath.Rel(portsDir, portDir)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[:len(parts)-1], "/")
	}
	return ""
}

func trimShellQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
