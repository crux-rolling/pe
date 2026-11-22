// Package repo manages the binary repository index.json published on kryx-linux/packages
// (release tag rolling-x86_64) and SHA-256 verified binary downloads.
package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kryx-linux/pe/internal/ui"
)

// Index matches the established index.json schema used for binary package releases,
// without ANRIX/USE-specific fields.
type Index struct {
	SchemaVersion int                `json:"schema_version"`
	Packages      map[string]Package `json:"packages"`
}

// Package groups all indexed versions of a package.
type Package struct {
	Latest   string  `json:"latest"`
	Versions []Entry `json:"versions"`
}

// Entry describes one binary package archive in index.json.
type Entry struct {
	Pkgname     string   `json:"pkgname"`
	Pkgver      string   `json:"pkgver"`
	Pkgrel      string   `json:"pkgrel"`
	Arch        string   `json:"arch"`
	Filename    string   `json:"filename"`
	URL         string   `json:"url"`
	SHA256      string   `json:"sha256"`
	Size        int64    `json:"size"`
	Description string   `json:"description,omitempty"`
	Depends     []string `json:"depends,omitempty"`
	Collection  string   `json:"collection,omitempty"`
}

// FullVersion returns "<pkgver>-<pkgrel>" (or "<pkgver>" if pkgrel is empty).
func (e Entry) FullVersion() string {
	if e.Pkgrel == "" {
		return e.Pkgver
	}
	return e.Pkgver + "-" + e.Pkgrel
}

// New creates an empty schema v1 Index.
func New() Index {
	return Index{
		SchemaVersion: 1,
		Packages:      make(map[string]Package),
	}
}

// Load reads and parses an index.json file from disk.
func Load(path string) (Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Index{}, fmt.Errorf("read repository index %q: %w", path, err)
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return Index{}, fmt.Errorf("parse repository index %q: %w", path, err)
	}
	if idx.SchemaVersion == 0 {
		idx.SchemaVersion = 1
	}
	if idx.Packages == nil {
		idx.Packages = make(map[string]Package)
	}
	return idx, nil
}

// Save writes index.json atomically with stable formatting.
func Save(path string, idx Index) error {
	if idx.SchemaVersion == 0 {
		idx.SchemaVersion = 1
	}
	if idx.Packages == nil {
		idx.Packages = make(map[string]Package)
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encode repository index: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create index directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pe-index-*")
	if err != nil {
		return fmt.Errorf("create temporary index file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary index file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set permissions on temporary index file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary index file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace repository index %q: %w", path, err)
	}
	return nil
}

// Merge inserts or updates package entries in idx, sorting versions and updating Latest.
func Merge(idx *Index, entries ...Entry) {
	if idx.SchemaVersion == 0 {
		idx.SchemaVersion = 1
	}
	if idx.Packages == nil {
		idx.Packages = make(map[string]Package)
	}
	for _, entry := range entries {
		if entry.Pkgname == "" || entry.Filename == "" {
			continue
		}
		group := idx.Packages[entry.Pkgname]
		replaced := false
		filtered := make([]Entry, 0, len(group.Versions)+1)
		for _, old := range group.Versions {
			sameArch := old.Arch == entry.Arch || old.Arch == "" || entry.Arch == ""
			sameVersion := old.Pkgver == entry.Pkgver && old.Pkgrel == entry.Pkgrel && sameArch
			sameFile := old.Filename == entry.Filename
			if sameVersion || sameFile {
				if !replaced {
					if entry.Arch == "" {
						entry.Arch = old.Arch
					}
					if entry.Description == "" {
						entry.Description = old.Description
					}
					if len(entry.Depends) == 0 {
						entry.Depends = old.Depends
					}
					if entry.Collection == "" {
						entry.Collection = old.Collection
					}
					filtered = append(filtered, entry)
					replaced = true
				}
				continue
			}
			filtered = append(filtered, old)
		}
		if !replaced {
			filtered = append(filtered, entry)
		}
		sort.Slice(filtered, func(i, j int) bool {
			return CompareVersion(filtered[i].Pkgver, filtered[i].Pkgrel, filtered[j].Pkgver, filtered[j].Pkgrel) < 0
		})
		group.Versions = filtered
		if len(filtered) > 0 {
			group.Latest = filtered[len(filtered)-1].FullVersion()
		}
		idx.Packages[entry.Pkgname] = group
	}
}

// Prune removes entries whose Filename is absent from existingFiles.
func Prune(idx *Index, existingFiles map[string]bool) int {
	if idx == nil || idx.Packages == nil {
		return 0
	}
	removed := 0
	for name, group := range idx.Packages {
		kept := make([]Entry, 0, len(group.Versions))
		for _, e := range group.Versions {
			if existingFiles[e.Filename] {
				kept = append(kept, e)
			} else {
				removed++
			}
		}
		if len(kept) == 0 {
			delete(idx.Packages, name)
			continue
		}
		sort.Slice(kept, func(i, j int) bool {
			return CompareVersion(kept[i].Pkgver, kept[i].Pkgrel, kept[j].Pkgver, kept[j].Pkgrel) < 0
		})
		group.Versions = kept
		group.Latest = kept[len(kept)-1].FullVersion()
		idx.Packages[name] = group
	}
	return removed
}

// Find returns the latest entry for name matching arch (or arch-agnostic entries).
func Find(idx Index, name, arch string) (Entry, bool) {
	group, ok := idx.Packages[name]
	if !ok || len(group.Versions) == 0 {
		return Entry{}, false
	}
	for i := len(group.Versions) - 1; i >= 0; i-- {
		e := group.Versions[i]
		if arch == "" || e.Arch == "" || e.Arch == "any" || e.Arch == arch {
			return e, true
		}
	}
	return Entry{}, false
}

// SortedNames returns all package names in idx sorted lexicographically.
func (idx Index) SortedNames() []string {
	names := make([]string, 0, len(idx.Packages))
	for name := range idx.Packages {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Sync downloads indexURL, validates it as a JSON index, and saves it atomically to destPath.
func Sync(indexURL, destPath string, timeout time.Duration) (Index, error) {
	return SyncWithLog(indexURL, destPath, timeout, nil, ui.Styler{})
}

// SyncWithLog downloads indexURL with optional progress logging, validates JSON, and saves to destPath.
func SyncWithLog(indexURL, destPath string, timeout time.Duration, out io.Writer, styler ui.Styler) (Index, error) {
	if err := ValidateHTTPURL(indexURL); err != nil {
		return Index{}, err
	}
	client := &http.Client{Timeout: effectiveTimeout(timeout)}
	resp, err := client.Get(indexURL)
	if err != nil {
		return Index{}, fmt.Errorf("download index %q: %w", indexURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Index{}, fmt.Errorf("download index %q: unexpected HTTP status %s", indexURL, resp.Status)
	}

	var reader io.Reader = resp.Body
	var prog *ui.Progress
	if out != nil {
		prog = ui.NewProgress(out, styler, "syncing index.json", resp.ContentLength)
		reader = io.TeeReader(resp.Body, prog)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		if prog != nil {
			prog.Fail()
		}
		return Index{}, fmt.Errorf("read index %q: %w", indexURL, err)
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		if prog != nil {
			prog.Fail()
		}
		return Index{}, fmt.Errorf("parse remote index %q: %w", indexURL, err)
	}
	if idx.SchemaVersion == 0 {
		idx.SchemaVersion = 1
	}
	if idx.Packages == nil {
		idx.Packages = make(map[string]Package)
	}
	if err := Save(destPath, idx); err != nil {
		if prog != nil {
			prog.Fail()
		}
		return Index{}, err
	}
	if prog != nil {
		prog.Finish(fmt.Sprintf("[%d packages]", len(idx.Packages)))
	}
	return idx, nil
}

// FetchBinary ensures the binary archive for entry exists in cacheDir and matches its SHA-256 checksum.
func FetchBinary(entry Entry, cacheDir string, timeout time.Duration, out io.Writer) (string, error) {
	return FetchBinaryWithStyle(entry, cacheDir, timeout, out, ui.NewStyler(ui.ColorNever, out))
}

// FetchBinaryWithStyle downloads (or reuses cached) binary archive with styled progress and checksum logs.
func FetchBinaryWithStyle(entry Entry, cacheDir string, timeout time.Duration, out io.Writer, styler ui.Styler) (string, error) {
	if out == nil {
		out = io.Discard
	}
	if filepath.Base(entry.Filename) != entry.Filename || entry.Filename == "." || entry.Filename == ".." {
		return "", fmt.Errorf("repository index entry %q has an unsafe filename", entry.Filename)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create binary cache dir %q: %w", cacheDir, err)
	}
	localPath := filepath.Join(cacheDir, entry.Filename)
	if info, err := os.Stat(localPath); err == nil && info.Mode().IsRegular() {
		if err := VerifySHA256(localPath, entry.SHA256); err == nil {
			fmt.Fprintf(out, "%s using cached %s (%s) %s\n",
				styler.BoldGreen("==>"),
				styler.Bold(entry.Filename),
				ui.FormatBytes(info.Size()),
				styler.Green("[sha256 ok]"),
			)
			return localPath, nil
		}
		fmt.Fprintf(out, "%s cached %s failed checksum; re-downloading\n",
			styler.BoldYellow("==>"),
			entry.Filename,
		)
		_ = os.Remove(localPath)
	}
	if entry.URL == "" {
		return "", fmt.Errorf("repository index entry %q has no download URL", entry.Filename)
	}
	if err := ValidateHTTPURL(entry.URL); err != nil {
		return "", err
	}

	if err := downloadFile(entry.URL, localPath, entry.Filename, entry.Size, entry.SHA256, timeout, out, styler); err != nil {
		return "", err
	}
	return localPath, nil
}

// SHA256 computes the lowercase hex SHA-256 digest of path.
func SHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %q for checksum: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifySHA256 checks that path matches expected (when expected is non-empty).
func VerifySHA256(path, expected string) error {
	if strings.TrimSpace(expected) == "" {
		return nil
	}
	actual, err := SHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("SHA-256 mismatch for %q: expected %s, got %s", path, expected, actual)
	}
	return nil
}

// ValidateHTTPURL ensures raw is a valid http or https URL with a host.
func ValidateHTTPURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q for %q; only http and https are supported", parsed.Scheme, raw)
	}
	if parsed.Host == "" {
		return fmt.Errorf("URL %q must include a host", raw)
	}
	return nil
}

// CompareVersion compares two CRUX package versions (verA, relA) vs (verB, relB).
// Returns -1 if A < B, 0 if A == B, +1 if A > B.
func CompareVersion(verA, relA, verB, relB string) int {
	if cmp := compareSegments(verA, verB); cmp != 0 {
		return cmp
	}
	return compareSegments(relA, relB)
}

func compareSegments(a, b string) int {
	if a == b {
		return 0
	}
	sa := tokenizeVersion(a)
	sb := tokenizeVersion(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		if sa[i] == sb[i] {
			continue
		}
		na, errA := strconv.Atoi(sa[i])
		nb, errB := strconv.Atoi(sb[i])
		if errA == nil && errB == nil {
			if na < nb {
				return -1
			}
			if na > nb {
				return 1
			}
			continue
		}
		if sa[i] < sb[i] {
			return -1
		}
		return 1
	}
	if len(sa) < len(sb) {
		return -1
	}
	if len(sa) > len(sb) {
		return 1
	}
	return 0
}

func tokenizeVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == '+'
	})
}

func downloadFile(rawURL, destPath, label string, expectedSize int64, expectedSHA256 string, timeout time.Duration, out io.Writer, styler ui.Styler) error {
	client := &http.Client{Timeout: effectiveTimeout(timeout)}
	resp, err := client.Get(rawURL)
	if err != nil {
		return fmt.Errorf("download %q: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %q: unexpected HTTP status %s", rawURL, resp.Status)
	}
	total := resp.ContentLength
	if total <= 0 && expectedSize > 0 {
		total = expectedSize
	}
	if label == "" {
		label = filepath.Base(destPath)
	}
	prog := ui.NewProgress(out, styler, "downloading "+label, total)

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".pe-dl-*")
	if err != nil {
		prog.Fail()
		return fmt.Errorf("create temp download file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, io.TeeReader(resp.Body, prog)); err != nil {
		prog.Fail()
		_ = tmp.Close()
		return fmt.Errorf("write download %q: %w", destPath, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		prog.Fail()
		_ = tmp.Close()
		return fmt.Errorf("set permissions on download %q: %w", destPath, err)
	}
	if err := tmp.Close(); err != nil {
		prog.Fail()
		return fmt.Errorf("close download %q: %w", destPath, err)
	}
	if err := VerifySHA256(tmpName, expectedSHA256); err != nil {
		prog.Fail()
		return err
	}
	if err := os.Rename(tmpName, destPath); err != nil {
		prog.Fail()
		return fmt.Errorf("finalize download %q: %w", destPath, err)
	}
	suffix := ""
	if strings.TrimSpace(expectedSHA256) != "" {
		suffix = "[sha256 ok]"
	}
	prog.Finish(suffix)
	return nil
}

func effectiveTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return 120 * time.Second
	}
	return d
}
