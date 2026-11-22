package srcmachine

import (
	"encoding/json"
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

// StageOptions configures repository indexing and GitHub-Release asset staging.
type StageOptions struct {
	PackagesDir string
	PortsDir    string
	IndexPath   string
	UploadDir   string // Optional: copy archives here with GitHub-safe filenames ('#' -> '.')
	BaseURL     string
	Arch        string
	Prune       bool
	ErrOut      io.Writer
}

// StageResult reports indexed archives and staged upload files.
type StageResult struct {
	IndexedCount int
	Archives     []string // Original CRUX archive filenames (with '#')
	UploadFiles  []string // Paths written to UploadDir (with '#' replaced by '.')
}

// Stage indexes all CRUX .pkg.tar.<ext> archives in PackagesDir into IndexPath
// (enriching metadata via LoadRegistry so both single-port and multi-collection
// trees work identically) and optionally prepares GitHub-safe upload copies.
func Stage(opts StageOptions) (StageResult, error) {
	if opts.ErrOut == nil {
		opts.ErrOut = io.Discard
	}
	if opts.BaseURL != "" {
		if err := repo.ValidateHTTPURL(opts.BaseURL); err != nil {
			return StageResult{}, err
		}
	}
	arch := opts.Arch
	if arch == "" {
		if runtime.GOARCH == "amd64" {
			arch = "x86_64"
		} else {
			arch = runtime.GOARCH
		}
	}

	idx := repo.New()
	if _, err := os.Stat(opts.IndexPath); err == nil {
		loaded, loadErr := repo.Load(opts.IndexPath)
		if loadErr != nil {
			return StageResult{}, loadErr
		}
		idx = loaded
	} else if !os.IsNotExist(err) {
		return StageResult{}, err
	}

	portMeta := make(map[string]ports.Port)
	if strings.TrimSpace(opts.PortsDir) != "" {
		if reg, err := LoadRegistry(opts.PortsDir, nil); err == nil {
			for _, e := range reg.Entries {
				if _, exists := portMeta[e.Port.Name]; !exists {
					portMeta[e.Port.Name] = e.Port
				}
			}
		}
	}

	paths, err := findArchives(opts.PackagesDir)
	if err != nil {
		return StageResult{}, err
	}
	if len(paths) == 0 {
		return StageResult{}, fmt.Errorf("no CRUX .pkg.tar.<ext> archives found in %s", opts.PackagesDir)
	}

	if opts.UploadDir != "" {
		if err := os.MkdirAll(opts.UploadDir, 0o755); err != nil {
			return StageResult{}, fmt.Errorf("create upload dir %s: %w", opts.UploadDir, err)
		}
	}

	var res StageResult
	present := make(map[string]bool)
	for _, p := range paths {
		filename := filepath.Base(p)
		name, fullVer, _, ok := archive.ParsePkgFilename(filename)
		if !ok {
			fmt.Fprintf(opts.ErrOut, "kryx-src: warning: ignoring archive %q (expected <name>#<version>-<release>.pkg.tar.<ext>)\n", filename)
			continue
		}
		ver, rel, _ := archive.SplitVersionRelease(fullVer)
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		sum, err := repo.SHA256(p)
		if err != nil {
			return res, fmt.Errorf("hash %s: %w", p, err)
		}

		entry := repo.Entry{
			Pkgname:  name,
			Pkgver:   ver,
			Pkgrel:   rel,
			Arch:     arch,
			Filename: filename,
			URL:      buildAssetURL(opts.BaseURL, filename),
			SHA256:   sum,
			Size:     info.Size(),
		}
		if pm, ok := portMeta[name]; ok {
			entry.Description = pm.Description
			entry.Depends = pm.Depends
			entry.MakeDepends = pm.MakeDepends
			entry.Collection = pm.Collection
		}

		present[filename] = true
		repo.Merge(&idx, entry)
		res.IndexedCount++
		res.Archives = append(res.Archives, filename)

		if opts.UploadDir != "" {
			safe := GitHubSafeAssetName(filename)
			dst := filepath.Join(opts.UploadDir, safe)
			if err := copyFile(p, dst); err != nil {
				return res, fmt.Errorf("stage upload asset %s: %w", safe, err)
			}
			res.UploadFiles = append(res.UploadFiles, dst)
		}
	}

	if res.IndexedCount == 0 {
		return res, fmt.Errorf("no valid CRUX archives found in %s", opts.PackagesDir)
	}

	if opts.Prune {
		repo.Prune(&idx, present)
	}
	if err := repo.Save(opts.IndexPath, idx); err != nil {
		return res, err
	}
	return res, nil
}

// GitHubSafeAssetName maps CRUX archive filenames containing '#' to the '.'
// form used by GitHub Releases asset uploads.
func GitHubSafeAssetName(filename string) string {
	return strings.ReplaceAll(filename, "#", ".")
}

type ghReleaseAssetsPayload struct {
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// RewriteReleaseURLs updates download URLs in indexPath using the JSON payload
// produced by `gh release view --json assets` and verifies that every archive in
// packagesDir (when non-empty) resolved to an uploaded release asset URL.
func RewriteReleaseURLs(indexPath, assetsJSONPath, packagesDir string) error {
	idx, err := repo.Load(indexPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(assetsJSONPath)
	if err != nil {
		return fmt.Errorf("read release assets JSON %s: %w", assetsJSONPath, err)
	}
	var payload ghReleaseAssetsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parse release assets JSON %s: %w", assetsJSONPath, err)
	}

	urlMap := make(map[string]string, len(payload.Assets))
	for _, a := range payload.Assets {
		if a.Name != "" && a.URL != "" {
			urlMap[a.Name] = a.URL
		}
	}

	for pkgName, grp := range idx.Packages {
		for i := range grp.Versions {
			fn := grp.Versions[i].Filename
			safe := GitHubSafeAssetName(fn)
			if u, ok := urlMap[fn]; ok {
				grp.Versions[i].URL = u
			} else if u, ok := urlMap[safe]; ok {
				grp.Versions[i].URL = u
			}
		}
		idx.Packages[pkgName] = grp
	}

	if packagesDir != "" {
		built, err := findArchives(packagesDir)
		if err != nil {
			return err
		}
		for _, p := range built {
			orig := filepath.Base(p)
			safe := GitHubSafeAssetName(orig)
			foundURL := ""
			for _, grp := range idx.Packages {
				for _, v := range grp.Versions {
					if v.Filename == orig {
						foundURL = v.URL
						break
					}
				}
			}
			if foundURL == "" || (!strings.Contains(foundURL, "/"+safe) && !strings.Contains(foundURL, "/"+url.PathEscape(orig))) {
				return fmt.Errorf("index.json URL for %s (%q) does not match uploaded asset %s", orig, foundURL, safe)
			}
		}
	}

	return repo.Save(indexPath, idx)
}

func findArchives(dir string) ([]string, error) {
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
	return strings.TrimRight(baseURL, "/") + "/" + url.PathEscape(filename)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
