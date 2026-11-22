// Command pe-index generates or updates index.json from CRUX .pkg.tar.<ext> archives
// for kryx-linux/ports rolling-x86_64 releases.
package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/kryx-linux/pe/internal/archive"
	"github.com/kryx-linux/pe/internal/ports"
	"github.com/kryx-linux/pe/internal/repo"
)

func main() {
	fs := flag.NewFlagSet("pe-index", flag.ExitOnError)
	indexPath := fs.String("index", "index.json", "JSON index to create or update")
	packagesDir := fs.String("packages", ".", "directory containing CRUX .pkg.tar.<ext> archives")
	portsDir := fs.String("ports", "", "optional CRUX ports directory to enrich descriptions and dependencies")
	baseURL := fs.String("base-url", "", "base URL for package downloads (e.g. https://github.com/kryx-linux/ports/releases/download/rolling-x86_64)")
	arch := fs.String("arch", defaultArch(), "package architecture")
	prune := fs.Bool("prune", false, "remove index entries whose archives are no longer present in --packages")

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if err := runIndex(*indexPath, *packagesDir, *portsDir, *baseURL, *arch, *prune, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runIndex(indexPath, packagesDir, portsDir, baseURL, arch string, prune bool, stderr io.Writer) error {
	if baseURL != "" {
		if err := repo.ValidateHTTPURL(baseURL); err != nil {
			return err
		}
	}

	idx := repo.New()
	if _, err := os.Stat(indexPath); err == nil {
		loaded, loadErr := repo.Load(indexPath)
		if loadErr != nil {
			return loadErr
		}
		idx = loaded
	} else if !os.IsNotExist(err) {
		return err
	}

	var portMeta map[string]ports.Port
	if strings.TrimSpace(portsDir) != "" {
		if discovered, err := ports.DiscoverAll(portsDir, nil); err == nil {
			portMeta = make(map[string]ports.Port, len(discovered))
			for _, p := range discovered {
				if _, exists := portMeta[p.Name]; !exists {
					portMeta[p.Name] = p
				}
			}
		}
	}

	paths, err := findPackageArchives(packagesDir)
	if err != nil {
		return err
	}

	seen := 0
	present := make(map[string]bool)
	for _, p := range paths {
		filename := filepath.Base(p)
		name, fullVer, _, ok := archive.ParsePkgFilename(filename)
		if !ok {
			fmt.Fprintf(stderr, "warning: ignoring archive with unexpected filename %q; expected <name>#<version>-<release>.pkg.tar.<ext>\n", filename)
			continue
		}
		ver, rel, _ := archive.SplitVersionRelease(fullVer)

		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		sum, err := repo.SHA256(p)
		if err != nil {
			fmt.Fprintf(stderr, "warning: cannot hash %q: %v\n", p, err)
			continue
		}

		entry := repo.Entry{
			Pkgname:  name,
			Pkgver:   ver,
			Pkgrel:   rel,
			Arch:     arch,
			Filename: filename,
			URL:      buildAssetURL(baseURL, filename),
			SHA256:   sum,
			Size:     info.Size(),
		}
		if pm, ok := portMeta[name]; ok {
			entry.Description = pm.Description
			entry.Depends = pm.Depends
			entry.Collection = pm.Collection
		}

		present[filename] = true
		repo.Merge(&idx, entry)
		seen++
	}

	if prune {
		repo.Prune(&idx, present)
	}
	if len(paths) == 0 {
		fmt.Fprintf(stderr, "warning: no CRUX .pkg.tar.<ext> archives found in %q; existing index versions were preserved\n", packagesDir)
	} else if seen == 0 {
		fmt.Fprintf(stderr, "warning: no valid CRUX package archives found in %q; existing index versions were preserved\n", packagesDir)
	}

	return repo.Save(indexPath, idx)
}

func findPackageArchives(dir string) ([]string, error) {
	var all []string
	for _, ext := range archive.SupportedExtensions {
		matches, err := filepath.Glob(filepath.Join(dir, "*.pkg.tar."+ext))
		if err != nil {
			return nil, err
		}
		all = append(all, matches...)
	}
	sort.Strings(all)
	return all, nil
}

func buildAssetURL(baseURL, filename string) string {
	if baseURL == "" {
		return filename
	}
	// Escape '#' as '%23' so URLs containing CRUX '#' delimiters are not truncated as URL fragments.
	return strings.TrimRight(baseURL, "/") + "/" + url.PathEscape(filename)
}

func defaultArch() string {
	if runtime.GOARCH == "amd64" {
		return "x86_64"
	}
	return runtime.GOARCH
}
