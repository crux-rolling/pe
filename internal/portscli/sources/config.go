// Package config parses and validates ports configuration files
// (/etc/ports/sources.list and drop-in files under /etc/ports/sources.list.d/*.list).
package sources

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// DefaultConfigPath is the primary flat-file configuration path.
	DefaultConfigPath = "/etc/ports/sources.list"
	// DefaultDropInDir is the directory for optional *.list drop-in files.
	DefaultDropInDir = "/etc/ports/sources.list.d"
	// DefaultPortsDir is the default base directory for synced port collections.
	DefaultPortsDir = "/usr/ports"
)

// Protocol defines a supported synchronization transport.
type Protocol string

const (
	ProtoGit     Protocol = "git"
	ProtoHTTPUp  Protocol = "httpup"
	ProtoRsync   Protocol = "rsync"
	ProtoTarball Protocol = "tarball"
)

// Source represents a single configured ports collection.
type Source struct {
	Protocol   Protocol
	Collection string
	URL        string
	DestDir    string
	OriginFile string
	LineNum    int
}

// Options controls where configuration is loaded from and how defaults resolve.
type Options struct {
	ConfigPath string
	DropInDir  string // If empty, derived as <dir(ConfigPath)>/sources.list.d
	PortsDir   string // If empty, defaults to DefaultPortsDir
}

// LintError represents a single syntax or validation issue in a config file.
type LintError struct {
	File    string
	Line    int
	Message string
}

func (e LintError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
	}
	if e.File != "" {
		return fmt.Sprintf("%s: %s", e.File, e.Message)
	}
	return e.Message
}

// Result holds the parsed sources, inspected files, and any lint errors found.
type Result struct {
	Sources []Source
	Files   []string
	Errors  []LintError
}

// OK reports whether parsing succeeded without any errors.
func (r *Result) OK() bool {
	return len(r.Errors) == 0
}

// Load reads and validates the primary sources.list and any drop-in *.list files.
// All syntax and semantic errors are collected in Result.Errors so callers such
// as `ports check` can report every issue in a single pass.
func Load(opts Options) *Result {
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = DefaultConfigPath
	}
	dropInDir := opts.DropInDir
	if dropInDir == "" {
		dropInDir = filepath.Join(filepath.Dir(cfgPath), "sources.list.d")
	}
	portsDir := opts.PortsDir
	if portsDir == "" {
		portsDir = DefaultPortsDir
	}

	res := &Result{}
	seen := make(map[string]Source)

	// Load primary sources.list file (required unless drop-ins exist? Report clearly if missing).
	primaryInfo, err := os.Stat(cfgPath)
	primaryExists := err == nil && !primaryInfo.IsDir()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Errors = append(res.Errors, LintError{
				File:    cfgPath,
				Message: "configuration file not found (create it or pass -c <path>)",
			})
		} else {
			res.Errors = append(res.Errors, LintError{
				File:    cfgPath,
				Message: fmt.Sprintf("cannot access configuration file: %v", err),
			})
		}
	} else if primaryInfo.IsDir() {
		res.Errors = append(res.Errors, LintError{
			File:    cfgPath,
			Message: "expected a regular file, got a directory",
		})
	} else {
		res.Files = append(res.Files, cfgPath)
		parseFile(cfgPath, portsDir, seen, res)
	}

	// Load optional drop-in directory (*.list), sorted lexicographically.
	if entries, err := os.ReadDir(dropInDir); err == nil {
		var listFiles []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasSuffix(name, ".list") && !strings.HasPrefix(name, ".") {
				listFiles = append(listFiles, filepath.Join(dropInDir, name))
			}
		}
		sort.Strings(listFiles)
		for _, file := range listFiles {
			res.Files = append(res.Files, file)
			parseFile(file, portsDir, seen, res)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		res.Errors = append(res.Errors, LintError{
			File:    dropInDir,
			Message: fmt.Sprintf("cannot read drop-in directory: %v", err),
		})
	}

	if primaryExists && len(res.Sources) == 0 && len(res.Errors) == 0 {
		res.Errors = append(res.Errors, LintError{
			File:    cfgPath,
			Message: "no active collections configured",
		})
	}

	return res
}

// ParseReader parses sources.list content from an io.Reader (useful for tests).
func ParseReader(r io.Reader, originFile, portsDir string) *Result {
	if portsDir == "" {
		portsDir = DefaultPortsDir
	}
	res := &Result{Files: []string{originFile}}
	seen := make(map[string]Source)
	parseLines(r, originFile, portsDir, seen, res)
	return res
}

func parseFile(path, portsDir string, seen map[string]Source, res *Result) {
	f, err := os.Open(path)
	if err != nil {
		res.Errors = append(res.Errors, LintError{
			File:    path,
			Message: fmt.Sprintf("cannot open file: %v", err),
		})
		return
	}
	defer f.Close()
	parseLines(f, path, portsDir, seen, res)
}

func parseLines(r io.Reader, originFile, portsDir string, seen map[string]Source, res *Result) {
	scanner := bufio.NewScanner(r)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		cleaned := stripComment(raw)
		if cleaned == "" {
			continue
		}

		fields := strings.Fields(cleaned)
		if len(fields) < 3 || len(fields) > 4 {
			res.Errors = append(res.Errors, LintError{
				File: originFile,
				Line: lineNum,
				Message: fmt.Sprintf(
					"expected 3 or 4 fields (<protocol> <collection> <url> [destination]), got %d",
					len(fields),
				),
			})
			continue
		}

		proto := Protocol(strings.ToLower(fields[0]))
		collection := fields[1]
		rawURL := fields[2]
		var destDir string
		if len(fields) == 4 {
			destDir = fields[3]
		} else {
			destDir = filepath.Join(portsDir, collection)
		}

		lineValid := true

		if !isValidProtocol(proto) {
			res.Errors = append(res.Errors, LintError{
				File:    originFile,
				Line:    lineNum,
				Message: fmt.Sprintf("unsupported protocol %q (must be git, httpup, rsync, or tarball)", fields[0]),
			})
			lineValid = false
		}

		if err := validateCollectionName(collection); err != nil {
			res.Errors = append(res.Errors, LintError{
				File:    originFile,
				Line:    lineNum,
				Message: err.Error(),
			})
			lineValid = false
		}

		if isValidProtocol(proto) {
			if err := validateURL(proto, rawURL); err != nil {
				res.Errors = append(res.Errors, LintError{
					File:    originFile,
					Line:    lineNum,
					Message: err.Error(),
				})
				lineValid = false
			}
		}

		if len(fields) == 4 {
			if !filepath.IsAbs(destDir) {
				res.Errors = append(res.Errors, LintError{
					File:    originFile,
					Line:    lineNum,
					Message: fmt.Sprintf("destination directory %q must be an absolute path", destDir),
				})
				lineValid = false
			} else {
				destDir = filepath.Clean(destDir)
			}
		} else {
			destDir = filepath.Clean(destDir)
		}

		if !lineValid {
			continue
		}

		if prev, exists := seen[collection]; exists {
			res.Errors = append(res.Errors, LintError{
				File: originFile,
				Line: lineNum,
				Message: fmt.Sprintf(
					"duplicate collection %q (previously defined at %s:%d)",
					collection, prev.OriginFile, prev.LineNum,
				),
			})
			continue
		}

		src := Source{
			Protocol:   proto,
			Collection: collection,
			URL:        rawURL,
			DestDir:    destDir,
			OriginFile: originFile,
			LineNum:    lineNum,
		}
		seen[collection] = src
		res.Sources = append(res.Sources, src)
	}

	if err := scanner.Err(); err != nil {
		res.Errors = append(res.Errors, LintError{
			File:    originFile,
			Message: fmt.Sprintf("error reading file: %v", err),
		})
	}
}

// stripComment removes full-line '#' comments and inline comments where '#' is
// preceded by whitespace. Preserving non-whitespace-preceded '#' allows URLs
// with httpup fragments such as https://host/repo/#subdir.
func stripComment(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	for i := 1; i < len(trimmed); i++ {
		if trimmed[i] == '#' && (trimmed[i-1] == ' ' || trimmed[i-1] == '\t') {
			return strings.TrimSpace(trimmed[:i])
		}
	}
	return trimmed
}

func isValidProtocol(p Protocol) bool {
	switch p {
	case ProtoGit, ProtoHTTPUp, ProtoRsync, ProtoTarball:
		return true
	default:
		return false
	}
}

func validateCollectionName(name string) error {
	if name == "" {
		return errors.New("collection name cannot be empty")
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("invalid collection name %q: path traversal not allowed", name)
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '+'
		if !ok {
			return fmt.Errorf("invalid collection name %q: contains disallowed character %q", name, r)
		}
	}
	return nil
}

func validateURL(proto Protocol, raw string) error {
	if strings.Contains(raw, "](") || strings.HasPrefix(raw, "[") {
		return fmt.Errorf("invalid URL %q: markdown links are not allowed; use a plain-text URL", raw)
	}

	switch proto {
	case ProtoHTTPUp, ProtoTarball:
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid %s URL %q: %v", proto, raw, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid %s URL %q: must use http:// or https:// with a host", proto, raw)
		}
		return nil

	case ProtoGit:
		// Support SCP-style git URLs (e.g. git@github.com:org/repo.git)
		if !strings.Contains(raw, "://") && isSCPStyle(raw) {
			return nil
		}
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid git URL %q: %v", raw, err)
		}
		switch u.Scheme {
		case "https", "http", "git", "ssh":
			if u.Host == "" {
				return fmt.Errorf("invalid git URL %q: missing host", raw)
			}
			return nil
		case "file":
			if u.Path == "" {
				return fmt.Errorf("invalid git file:// URL %q: missing path", raw)
			}
			return nil
		default:
			return fmt.Errorf("invalid git URL %q: unsupported scheme %q (use https://, http://, git://, ssh://, file://, or user@host:path)", raw, u.Scheme)
		}

	case ProtoRsync:
		// Support rsync://host/module, host::module/path, or user@host:path
		if strings.HasPrefix(raw, "rsync://") {
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				return fmt.Errorf("invalid rsync URL %q: missing host", raw)
			}
			return nil
		}
		if strings.Contains(raw, "::") {
			parts := strings.SplitN(raw, "::", 2)
			if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return fmt.Errorf("invalid rsync daemon URL %q: expected host::module", raw)
			}
			return nil
		}
		if isSCPStyle(raw) {
			return nil
		}
		return fmt.Errorf("invalid rsync URL %q: expected rsync://host/path, host::module/path, or user@host:path", raw)
	}

	return fmt.Errorf("unknown protocol %q", proto)
}

func isSCPStyle(s string) bool {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon >= len(s)-1 {
		return false
	}
	hostPart := s[:colon]
	// Avoid matching schemes or paths containing '/' before ':'
	if strings.ContainsAny(hostPart, "/\\") {
		return false
	}
	return true
}

// Filter returns the subset of sources matching the requested collection names.
// If names is empty, all sources are returned. Returns an error listing any
// unknown collection names.
func Filter(sources []Source, names []string) ([]Source, error) {
	if len(names) == 0 {
		return sources, nil
	}
	byName := make(map[string]Source, len(sources))
	for _, s := range sources {
		byName[s.Collection] = s
	}

	var matched []Source
	var missing []string
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if src, ok := byName[name]; ok {
			matched = append(matched, src)
		} else {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		avail := make([]string, 0, len(sources))
		for _, s := range sources {
			avail = append(avail, s.Collection)
		}
		return nil, fmt.Errorf(
			"unknown collection(s): %s (configured: %s)",
			strings.Join(missing, ", "),
			strings.Join(avail, ", "),
		)
	}
	return matched, nil
}
