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
)

// Port holds parsed metadata from a CRUX Pkgfile.
type Port struct {
	Name        string
	Version     string
	Release     string
	Collection  string
	Dir         string
	Description string
	URL         string
	Maintainer  string
	Packager    string
	Depends     []string
	Source      []string
	Renames     []string
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

// ListEntries discovers all Pkgfiles under portsDir in place, returning sorted
// relative port paths matching CRUX `ports -l` (e.g. "core/zlib", "opt/git").
func ListEntries(portsDir string, collections []string) ([]string, error) {
	if _, err := os.Stat(portsDir); err != nil {
		return nil, fmt.Errorf("ports directory %q not found: %w", portsDir, err)
	}

	var entries []string
	seen := make(map[string]bool)

	addFromRoot := func(searchRoot, prefix string) error {
		return filepath.WalkDir(searchRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if strings.HasPrefix(name, ".") && path != searchRoot {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "Pkgfile" {
				return nil
			}
			dir := filepath.Dir(path)
			rel, err := filepath.Rel(portsDir, dir)
			if err != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				rel = filepath.Base(portsDir)
			}
			if prefix != "" && !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
				rel = prefix + "/" + rel
			}
			if !seen[rel] {
				seen[rel] = true
				entries = append(entries, rel)
			}
			return nil
		})
	}

	if len(collections) > 0 {
		foundAnyCollectionDir := false
		for _, col := range collections {
			colDir := filepath.Join(portsDir, col)
			if info, err := os.Stat(colDir); err == nil && info.IsDir() {
				foundAnyCollectionDir = true
				_ = addFromRoot(colDir, col)
			}
		}
		if !foundAnyCollectionDir {
			_ = addFromRoot(portsDir, "")
		}
	} else {
		_ = addFromRoot(portsDir, "")
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

// Find locates a single port by name (or "collection/name") honoring collections priority order.
func Find(portsDir string, collections []string, target string) (Port, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return Port{}, fmt.Errorf("empty port name")
	}

	// Explicit collection/port path
	if strings.Contains(target, "/") {
		dir := filepath.Join(portsDir, filepath.FromSlash(target))
		if _, err := os.Stat(filepath.Join(dir, "Pkgfile")); err == nil {
			return ParsePkgfile(dir, portsDir)
		}
		return Port{}, fmt.Errorf("port %q not found in %s", target, portsDir)
	}

	// Priority order by configured collections
	for _, col := range collections {
		dir := filepath.Join(portsDir, col, target)
		if _, err := os.Stat(filepath.Join(dir, "Pkgfile")); err == nil {
			return ParsePkgfile(dir, portsDir)
		}
	}

	// Direct flat layout <portsDir>/<target>/Pkgfile
	direct := filepath.Join(portsDir, target)
	if _, err := os.Stat(filepath.Join(direct, "Pkgfile")); err == nil {
		return ParsePkgfile(direct, portsDir)
	}

	// Fallback scan of all collections under portsDir
	dirs, err := os.ReadDir(portsDir)
	if err == nil {
		var names []string
		for _, d := range dirs {
			if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
				names = append(names, d.Name())
			}
		}
		sort.Strings(names)
		for _, col := range names {
			dir := filepath.Join(portsDir, col, target)
			if _, err := os.Stat(filepath.Join(dir, "Pkgfile")); err == nil {
				return ParsePkgfile(dir, portsDir)
			}
		}
	}

	return Port{}, fmt.Errorf("port %q not found in %s", target, portsDir)
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
	visited := make(map[string]bool)
	visiting := make(map[string]bool)
	missingSet := make(map[string]bool)

	var dfs func(name string, isRoot bool) error
	dfs = func(name string, isRoot bool) error {
		if visited[name] || visiting[name] {
			return nil
		}
		p, findErr := Find(portsDir, collections, name)
		if findErr != nil {
			if isRoot {
				return findErr
			}
			if !missingSet[name] {
				missingSet[name] = true
				missing = append(missing, name)
			}
			return nil
		}
		visiting[p.Name] = true
		for _, dep := range p.Depends {
			if err := dfs(dep, false); err != nil {
				return err
			}
		}
		delete(visiting, p.Name)
		if !visited[p.Name] {
			visited[p.Name] = true
			ordered = append(ordered, p)
		}
		return nil
	}

	for _, t := range targets {
		if err := dfs(t, true); err != nil {
			return nil, nil, err
		}
	}
	sort.Strings(missing)
	return ordered, missing, nil
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
			p.Depends = parseDepends(val)
		}
	}
	if err := scanner.Err(); err != nil {
		return Port{}, err
	}
	return p, nil
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
