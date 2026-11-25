package srcmachine

import (
	"bufio"
	"debug/elf"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/pkgdb"
)

// MissingLibIssue describes an unresolved ELF DT_NEEDED shared library inside a rootfs.
type MissingLibIssue struct {
	Binary  string `json:"binary"`
	Library string `json:"library"`
}

func (m MissingLibIssue) String() string {
	return fmt.Sprintf("%s requires missing shared library %s", m.Binary, m.Library)
}

// DiscoverRuntimeDeps inspects a built CRUX package archive (.pkg.tar.<ext>) using
// Go's stdlib debug/elf and shebang analysis, resolving every required DT_NEEDED
// shared library and script interpreter back to its owning package in root's
// /var/lib/pkg/db. Returns deduplicated, sorted runtime package names (excluding self).
func DiscoverRuntimeDeps(archivePath, root string) ([]string, error) {
	pkgName, _, _, ok := archive.ParsePkgFilename(filepath.Base(archivePath))
	if !ok {
		return nil, fmt.Errorf("invalid package filename %q", filepath.Base(archivePath))
	}

	db, err := pkgdb.Open(root)
	if err != nil {
		return nil, fmt.Errorf("open pkgdb at %s: %w", root, err)
	}

	soOwner := make(map[string]string)
	pathOwner := make(map[string]string)
	for name, info := range db.Packages {
		for f := range info.Files {
			clean := strings.TrimPrefix(filepath.ToSlash(f), "/")
			if clean == "" || strings.HasSuffix(clean, "/") {
				continue
			}
			pathOwner[clean] = name
			if isStandardLibPath(clean) {
				base := filepath.Base(clean)
				if strings.Contains(base, ".so") {
					if _, exists := soOwner[base]; !exists {
						soOwner[base] = name
					}
				}
			}
		}
	}

	tmpDir, err := os.MkdirTemp("", "kryx-audit-deps-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	if err := archive.Extract(archivePath, tmpDir, nil, nil, true, io.Discard, io.Discard); err != nil {
		return nil, fmt.Errorf("extract %s for dep audit: %w", archivePath, err)
	}

	selfProvidedSO := make(map[string]bool)
	var elfPaths []string
	var scriptPaths []string

	err = filepath.WalkDir(tmpDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, rerr := filepath.Rel(tmpDir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		base := filepath.Base(rel)
		if isStandardLibPath(rel) && strings.Contains(base, ".so") {
			selfProvidedSO[base] = true
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		fi, serr := d.Info()
		if serr != nil || !fi.Mode().IsRegular() {
			return nil
		}
		magic := make([]byte, 4)
		f, oerr := os.Open(p)
		if oerr != nil {
			return nil
		}
		n, _ := f.Read(magic)
		_ = f.Close()
		if n == 4 && magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F' {
			elfPaths = append(elfPaths, p)
		} else if n >= 2 && magic[0] == '#' && magic[1] == '!' && fi.Mode()&0o111 != 0 {
			scriptPaths = append(scriptPaths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	depSet := make(map[string]bool)

	for _, p := range elfPaths {
		ef, oerr := elf.Open(p)
		if oerr != nil {
			continue
		}
		libs, _ := ef.ImportedLibraries()
		_ = ef.Close()
		for _, lib := range libs {
			base := filepath.Base(lib)
			if selfProvidedSO[base] {
				continue
			}
			if owner, ok := soOwner[base]; ok && owner != pkgName {
				depSet[owner] = true
			} else if strings.HasPrefix(lib, "/") {
				if owner, ok := pathOwner[strings.TrimPrefix(lib, "/")]; ok && owner != pkgName {
					depSet[owner] = true
				}
			}
		}
	}

	for _, p := range scriptPaths {
		interp := parseShebangInterpreter(p)
		if interp == "" {
			continue
		}
		candidates := interpreterPaths(interp)
		for _, cand := range candidates {
			if owner, ok := pathOwner[cand]; ok && owner != pkgName {
				depSet[owner] = true
				break
			}
		}
	}

	deps := make([]string, 0, len(depSet))
	for d := range depSet {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return deps, nil
}

// VerifyRoot scans a staged rootfs for unresolved ELF DT_NEEDED shared libraries
// across standard binary and library directories (including PAM /lib/security plugins).
func VerifyRoot(root string) ([]MissingLibIssue, error) {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("verify-root: invalid root directory %q", root)
	}

	libDirs := []string{"lib", "lib64", "usr/lib", "usr/lib64"}
	for _, extra := range loadLdSoConfDirs(root) {
		libDirs = append(libDirs, strings.TrimPrefix(filepath.ToSlash(extra), "/"))
	}

	presentSO := make(map[string]bool)
	for _, relDir := range libDirs {
		absDir := filepath.Join(root, filepath.FromSlash(relDir))
		entries, derr := os.ReadDir(absDir)
		if derr != nil {
			continue
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".so") {
				presentSO[e.Name()] = true
			}
		}
	}

	scanDirs := []string{"bin", "sbin", "usr/bin", "usr/sbin", "lib", "lib64", "usr/lib", "usr/lib64", "lib/security"}
	var issues []MissingLibIssue
	seenBin := make(map[string]bool)

	for _, relDir := range scanDirs {
		absDir := filepath.Join(root, filepath.FromSlash(relDir))
		entries, derr := os.ReadDir(absDir)
		if derr != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			absPath := filepath.Join(absDir, e.Name())
			relPath := "/" + relDir + "/" + e.Name()
			if seenBin[relPath] {
				continue
			}
			seenBin[relPath] = true

			ef, oerr := elf.Open(absPath)
			if oerr != nil {
				continue
			}
			libs, _ := ef.ImportedLibraries()
			_ = ef.Close()
			for _, lib := range libs {
				if strings.HasPrefix(lib, "/") {
					if _, serr := os.Stat(filepath.Join(root, strings.TrimPrefix(lib, "/"))); serr != nil {
						issues = append(issues, MissingLibIssue{Binary: relPath, Library: lib})
					}
					continue
				}
				if !presentSO[lib] {
					issues = append(issues, MissingLibIssue{Binary: relPath, Library: lib})
				}
			}
		}
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Binary != issues[j].Binary {
			return issues[i].Binary < issues[j].Binary
		}
		return issues[i].Library < issues[j].Library
	})
	return issues, nil
}

func isStandardLibPath(rel string) bool {
	for _, prefix := range []string{"lib/", "lib64/", "usr/lib/", "usr/lib64/", "lib/security/"} {
		if strings.HasPrefix(rel, prefix) {
			rest := strings.TrimPrefix(rel, prefix)
			if !strings.Contains(rest, "/") {
				return true
			}
		}
	}
	return false
}

func parseShebangInterpreter(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	r := bufio.NewReader(f)
	line, err := r.ReadString('\n')
	if err != nil && len(line) == 0 {
		return ""
	}
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "#!")))
	if len(fields) == 0 {
		return ""
	}
	if filepath.Base(fields[0]) == "env" {
		for _, tok := range fields[1:] {
			if !strings.HasPrefix(tok, "-") && !strings.Contains(tok, "=") {
				return tok
			}
		}
		return ""
	}
	return fields[0]
}

func interpreterPaths(interp string) []string {
	if strings.HasPrefix(interp, "/") {
		return []string{strings.TrimPrefix(filepath.ToSlash(filepath.Clean(interp)), "/")}
	}
	return []string{
		"bin/" + interp,
		"usr/bin/" + interp,
		"sbin/" + interp,
		"usr/sbin/" + interp,
	}
}

func loadLdSoConfDirs(root string) []string {
	var dirs []string
	var parseFile func(p string)
	parseFile = func(p string) {
		f, err := os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "include ") {
				pat := strings.TrimSpace(strings.TrimPrefix(line, "include "))
				if !filepath.IsAbs(pat) {
					pat = filepath.Join(filepath.Dir(p), pat)
				} else {
					pat = filepath.Join(root, strings.TrimPrefix(pat, "/"))
				}
				matches, _ := filepath.Glob(pat)
				for _, m := range matches {
					parseFile(m)
				}
				continue
			}
			dirs = append(dirs, line)
		}
	}
	parseFile(filepath.Join(root, "etc", "ld.so.conf"))
	return dirs
}

func mergeUniqueDeps(base, extra []string) []string {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, list := range [][]string{base, extra} {
		for _, d := range list {
			d = strings.TrimSpace(d)
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// ShardTag maps a package's collection and name onto a GitHub-Release-safe shard tag
// (staying well under GitHub's 1,000 assets/release cap across all 2,052+ ports).
func ShardTag(baseTag, collection, pkgName string) string {
	baseTag = strings.TrimSpace(baseTag)
	if baseTag == "" {
		baseTag = "rolling-x86_64"
	}
	switch strings.ToLower(strings.TrimSpace(collection)) {
	case "core":
		return baseTag + "-core"
	case "opt":
		return baseTag + "-opt"
	case "xorg":
		return baseTag + "-xorg"
	case "compat-32":
		return baseTag + "-compat32"
	case "contrib":
		if pkgName != "" {
			c := strings.ToLower(pkgName[:1])[0]
			if c >= 'n' && c <= 'z' {
				return baseTag + "-contrib-nz"
			}
		}
		return baseTag + "-contrib-am"
	default:
		return baseTag + "-kryx"
	}
}
