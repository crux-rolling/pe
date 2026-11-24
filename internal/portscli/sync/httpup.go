package sync

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

const (
	httpupDownloadWorkers = 8
	maxRepoManifestBytes  = 32 << 20 // 32 MiB safety cap for REPO index
	httpupUserAgent       = "ports/1.0"
)

// RepoEntryType indicates the filesystem entry kind in an httpup REPO manifest.
type RepoEntryType byte

const (
	EntryDir     RepoEntryType = 'd'
	EntryFile    RepoEntryType = 'f'
	EntrySymlink RepoEntryType = 'l'
)

// RepoEntry represents one parsed line from an httpup REPO file.
type RepoEntry struct {
	Type     RepoEntryType
	RelPath  string
	Mode     os.FileMode // 0 means unspecified (use default 0755/0644)
	MD5      string      // lowercase 32-char hex MD5 when provided
	Size     int64       // -1 when unspecified
	LinkDest string      // symlink target for EntrySymlink
}

// RepoManifest holds the parsed contents of a remote httpup REPO file.
type RepoManifest struct {
	Dirs     []RepoEntry
	Files    []RepoEntry
	Symlinks []RepoEntry
}

// NewHTTPClient builds a pooled HTTP/2-capable client tuned for concurrent
// httpup port synchronization.
func NewHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   60 * time.Second,
	}
}

func syncHTTPUp(ctx context.Context, client *http.Client, src sources.Source) (SyncOutcome, error) {
	if client == nil {
		client = NewHTTPClient()
	}

	baseURL, subpath, err := splitHTTPUpURL(src.URL)
	if err != nil {
		return SyncOutcome{}, err
	}

	// 1. Fetch and parse remote REPO manifest.
	repoURL := joinRemoteURL(baseURL, "REPO")
	manifest, err := fetchRepoManifest(ctx, client, repoURL, subpath)
	if err != nil {
		return SyncOutcome{}, err
	}

	// 2. Ensure destination root and manifest directories exist.
	if err := os.MkdirAll(src.DestDir, 0o755); err != nil {
		return SyncOutcome{}, fmt.Errorf("cannot create destination %s: %w", src.DestDir, err)
	}

	expectedDirs := make(map[string]struct{}, len(manifest.Dirs)+len(manifest.Files))
	for _, d := range manifest.Dirs {
		expectedDirs[d.RelPath] = struct{}{}
		addParentDirs(expectedDirs, d.RelPath)
		fullDir := filepath.Join(src.DestDir, filepath.FromSlash(d.RelPath))
		mode := d.Mode
		if mode == 0 {
			mode = 0o755
		}
		if err := ensureDir(fullDir, mode); err != nil {
			return SyncOutcome{}, err
		}
	}
	for _, f := range manifest.Files {
		addParentDirs(expectedDirs, f.RelPath)
	}
	for _, l := range manifest.Symlinks {
		addParentDirs(expectedDirs, l.RelPath)
	}

	// 3. Diff manifest files against local tree to determine downloads needed.
	type downloadJob struct {
		entry   RepoEntry
		isNew   bool
		remote  string
		local   string
	}

	expectedFiles := make(map[string]struct{}, len(manifest.Files)+len(manifest.Symlinks))
	var jobs []downloadJob

	for _, f := range manifest.Files {
		expectedFiles[f.RelPath] = struct{}{}
		localPath := filepath.Join(src.DestDir, filepath.FromSlash(f.RelPath))
		need, isNew, err := needsDownload(localPath, f)
		if err != nil {
			return SyncOutcome{}, err
		}
		if need {
			remoteRel := f.RelPath
			if subpath != "" {
				remoteRel = path.Join(subpath, f.RelPath)
			}
			jobs = append(jobs, downloadJob{
				entry:  f,
				isNew:  isNew,
				remote: joinRemoteURL(baseURL, remoteRel),
				local:  localPath,
			})
		} else if f.Mode != 0 {
			if info, statErr := os.Lstat(localPath); statErr == nil && info.Mode().Perm() != f.Mode.Perm() {
				_ = os.Chmod(localPath, f.Mode.Perm())
			}
		}
	}

	// 4. Download new and modified files concurrently.
	var addedCount int64
	var updatedCount int64

	if len(jobs) > 0 {
		workerCount := httpupDownloadWorkers
		if len(jobs) < workerCount {
			workerCount = len(jobs)
		}

		dlCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		jobCh := make(chan downloadJob)
		var firstErr error
		var errOnce stdsync.Once
		var wg stdsync.WaitGroup

		for i := 0; i < workerCount; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for job := range jobCh {
					if dlCtx.Err() != nil {
						return
					}
					if err := downloadFileWithRetry(dlCtx, client, job.remote, job.local, job.entry); err != nil {
						errOnce.Do(func() {
							firstErr = err
							cancel()
						})
						return
					}
					if job.isNew {
						atomic.AddInt64(&addedCount, 1)
					} else {
						atomic.AddInt64(&updatedCount, 1)
					}
				}
			}()
		}

	enqueueLoop:
		for _, job := range jobs {
			select {
			case <-dlCtx.Done():
				break enqueueLoop
			case jobCh <- job:
			}
		}
		close(jobCh)
		wg.Wait()

		if firstErr != nil {
			return SyncOutcome{}, firstErr
		}
		if ctx.Err() != nil {
			return SyncOutcome{}, errors.New("sync canceled")
		}
	}

	// 5. Sync symlinks if any exist in the manifest.
	for _, l := range manifest.Symlinks {
		expectedFiles[l.RelPath] = struct{}{}
		localPath := filepath.Join(src.DestDir, filepath.FromSlash(l.RelPath))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return SyncOutcome{}, fmt.Errorf("cannot create parent directory for %s: %w", l.RelPath, err)
		}
		existing, err := os.Readlink(localPath)
		if err == nil && existing == l.LinkDest {
			continue
		}
		isNew := errors.Is(err, os.ErrNotExist)
		_ = os.RemoveAll(localPath)
		if err := os.Symlink(l.LinkDest, localPath); err != nil {
			return SyncOutcome{}, fmt.Errorf("cannot create symlink %s: %w", l.RelPath, err)
		}
		if isNew {
			addedCount++
		} else {
			updatedCount++
		}
	}

	// 6. Prune local files and directories removed upstream.
	prunedCount, err := pruneLocalTree(src.DestDir, expectedFiles, expectedDirs)
	if err != nil {
		return SyncOutcome{}, err
	}

	if addedCount == 0 && updatedCount == 0 && prunedCount == 0 {
		return SyncOutcome{Skipped: true, Detail: "up-to-date"}, nil
	}

	var parts []string
	if addedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d added", addedCount))
	}
	if updatedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", updatedCount))
	}
	if prunedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d pruned", prunedCount))
	}
	return SyncOutcome{Detail: strings.Join(parts, ", ")}, nil
}

func splitHTTPUpURL(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("invalid httpup URL %q: %w", raw, err)
	}
	subpath := strings.Trim(u.Fragment, "/")
	u.Fragment = ""
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u, subpath, nil
}

func joinRemoteURL(base *url.URL, relPath string) string {
	cloned := *base
	cleanRel := strings.TrimPrefix(path.Clean("/"+relPath), "/")
	cloned.Path = path.Join(base.Path, cleanRel)
	if !strings.HasPrefix(cloned.Path, "/") {
		cloned.Path = "/" + cloned.Path
	}
	return cloned.String()
}

func fetchRepoManifest(ctx context.Context, client *http.Client, repoURL, subpath string) (*RepoManifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot build request for REPO manifest: %w", err)
	}
	req.Header.Set("User-Agent", httpupUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, sanitizeHTTPError("fetching REPO manifest", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote REPO manifest returned HTTP %d (%s)", resp.StatusCode, repoURL)
	}

	limited := io.LimitReader(resp.Body, maxRepoManifestBytes)
	manifest, err := ParseRepoManifest(limited, subpath)
	if err != nil {
		return nil, fmt.Errorf("invalid REPO manifest from %s: %w", repoURL, err)
	}
	return manifest, nil
}

// ParseRepoManifest parses a CRUX httpup REPO manifest stream.
// It supports standard httpup-repgen records:
//   - d:<path> | d:<mode>:<path>
//   - f:<md5>:<path> | f:<mode>:<md5>:<path> | f:<md5>:<mode>:<path>
//   - f:<size>:<md5>:<path> | f:<md5>:<size>:<path> | f:<mode>:<size>:<md5>:<path> | f:<path>
//   - l:<target>:<path> | l:<mode>:<target>:<path>
func ParseRepoManifest(r io.Reader, subpath string) (*RepoManifest, error) {
	manifest := &RepoManifest{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	nonEmpty := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		nonEmpty++

		// Detect HTML error pages served with 200 OK.
		if nonEmpty == 1 && (strings.HasPrefix(strings.ToLower(line), "<!doctype") || strings.HasPrefix(strings.ToLower(line), "<html")) {
			return nil, errors.New("received HTML instead of an httpup REPO manifest")
		}

		entry, skip, err := parseRepoLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		if skip {
			continue
		}

		// Filter by optional #subpath fragment.
		if subpath != "" {
			if entry.RelPath == subpath {
				continue
			}
			prefix := subpath + "/"
			if !strings.HasPrefix(entry.RelPath, prefix) {
				continue
			}
			entry.RelPath = strings.TrimPrefix(entry.RelPath, prefix)
		}

		switch entry.Type {
		case EntryDir:
			manifest.Dirs = append(manifest.Dirs, entry)
		case EntryFile:
			manifest.Files = append(manifest.Files, entry)
		case EntrySymlink:
			manifest.Symlinks = append(manifest.Symlinks, entry)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading REPO manifest: %w", err)
	}
	if nonEmpty == 0 {
		return nil, errors.New("REPO manifest is empty")
	}
	return manifest, nil
}

func parseRepoLine(line string) (RepoEntry, bool, error) {
	if len(line) < 2 || line[1] != ':' {
		// Support fallback "<32-hex-md5>  <path>" format if encountered.
		fields := strings.Fields(line)
		if len(fields) == 2 && isMD5Hex(fields[0]) {
			rel, ok, err := cleanRepoPath(fields[1])
			if err != nil || !ok {
				return RepoEntry{}, !ok, err
			}
			return RepoEntry{
				Type:    EntryFile,
				RelPath: rel,
				MD5:     strings.ToLower(fields[0]),
				Size:    -1,
			}, false, nil
		}
		return RepoEntry{}, false, fmt.Errorf("unrecognized REPO record %q", line)
	}

	kind := RepoEntryType(line[0])
	rest := line[2:]

	switch kind {
	case EntryDir:
		mode := os.FileMode(0)
		rawPath := rest
		if idx := strings.IndexByte(rest, ':'); idx > 0 && isOctalMode(rest[:idx]) {
			mode = parseOctalMode(rest[:idx])
			rawPath = rest[idx+1:]
		}
		rel, ok, err := cleanRepoPath(rawPath)
		if err != nil || !ok {
			return RepoEntry{}, !ok, err
		}
		return RepoEntry{
			Type:    EntryDir,
			RelPath: rel,
			Mode:    mode,
			Size:    -1,
		}, false, nil

	case EntryFile:
		entry, ok, err := parseFileRecord(rest)
		return entry, !ok, err

	case EntrySymlink:
		// Formats: l:<target>:<path> or l:<mode>:<target>:<path>
		parts := strings.SplitN(rest, ":", 3)
		var target, rawPath string
		if len(parts) == 3 && isOctalMode(parts[0]) {
			target = parts[1]
			rawPath = parts[2]
		} else if len(parts) >= 2 {
			target = parts[0]
			rawPath = strings.Join(parts[1:], ":")
		} else {
			return RepoEntry{}, false, fmt.Errorf("malformed symlink record %q", line)
		}
		rel, ok, err := cleanRepoPath(rawPath)
		if err != nil || !ok {
			return RepoEntry{}, !ok, err
		}
		return RepoEntry{
			Type:     EntrySymlink,
			RelPath:  rel,
			LinkDest: target,
			Size:     -1,
		}, false, nil

	default:
		return RepoEntry{}, false, fmt.Errorf("unsupported REPO entry type %q", string(kind))
	}
}

func parseFileRecord(rest string) (RepoEntry, bool, error) {
	entry := RepoEntry{
		Type: EntryFile,
		Size: -1,
	}

	// Tokenize leading metadata fields separated by ':' while keeping the final
	// path intact even if a filename itself contains ':'.
	rem := rest

	// Try up to 3 metadata tokens (mode, size, md5 in any standard order).
	for i := 0; i < 3; i++ {
		idx := strings.IndexByte(rem, ':')
		if idx <= 0 {
			break
		}
		tok := rem[:idx]
		after := rem[idx+1:]
		if after == "" {
			break
		}

		switch {
		case entry.MD5 == "" && isMD5Hex(tok):
			entry.MD5 = strings.ToLower(tok)
			rem = after
		case entry.Mode == 0 && isOctalMode(tok) && (hasNextMetadata(after) || entry.MD5 != ""):
			entry.Mode = parseOctalMode(tok)
			rem = after
		case entry.Size < 0 && isDecimalSize(tok) && (hasNextMetadata(after) || entry.MD5 != ""):
			if sz, err := strconv.ParseInt(tok, 10, 64); err == nil {
				entry.Size = sz
				rem = after
			} else {
				break
			}
		default:
			i = 3 // Stop consuming metadata; remainder is the path.
		}
	}

	rel, ok, err := cleanRepoPath(rem)
	if err != nil || !ok {
		return RepoEntry{}, false, err
	}
	entry.RelPath = rel
	return entry, true, nil
}

// hasNextMetadata reports whether s starts with another colon-delimited md5,
// octal mode, or decimal size token (meaning the current token is metadata, not
// a directory/filename).
func hasNextMetadata(s string) bool {
	idx := strings.IndexByte(s, ':')
	if idx <= 0 {
		return false
	}
	tok := s[:idx]
	return isMD5Hex(tok) || isOctalMode(tok) || isDecimalSize(tok)
}

func isMD5Hex(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func isOctalMode(s string) bool {
	if len(s) != 3 && len(s) != 4 {
		return false
	}
	if len(s) == 4 && s[0] != '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

func parseOctalMode(s string) os.FileMode {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0
	}
	return os.FileMode(v) & os.ModePerm
}

func isDecimalSize(s string) bool {
	if len(s) == 0 || len(s) > 15 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func cleanRepoPath(raw string) (string, bool, error) {
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", false, fmt.Errorf("invalid empty or NUL path in REPO manifest")
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") {
		return "", false, fmt.Errorf("unsafe absolute path %q in REPO manifest", raw)
	}
	cleaned := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
	if cleaned == "." || cleaned == "" {
		return "", false, nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false, fmt.Errorf("unsafe path traversal %q in REPO manifest", raw)
	}
	return cleaned, true, nil
}

func addParentDirs(dirs map[string]struct{}, rel string) {
	dir := path.Dir(rel)
	for dir != "." && dir != "/" && dir != "" {
		dirs[dir] = struct{}{}
		dir = path.Dir(dir)
	}
}

func ensureDir(fullPath string, mode os.FileMode) error {
	info, err := os.Lstat(fullPath)
	if err == nil {
		if !info.IsDir() {
			if rmErr := os.Remove(fullPath); rmErr != nil {
				return fmt.Errorf("cannot replace non-directory %s: %w", fullPath, rmErr)
			}
		} else {
			return nil
		}
	}
	if err := os.MkdirAll(fullPath, mode.Perm()); err != nil {
		return fmt.Errorf("cannot create directory %s: %w", fullPath, err)
	}
	return nil
}

func needsDownload(localPath string, entry RepoEntry) (need bool, isNew bool, err error) {
	info, statErr := os.Lstat(localPath)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return true, true, nil
		}
		return false, false, fmt.Errorf("cannot stat %s: %w", localPath, statErr)
	}
	if !info.Mode().IsRegular() {
		return true, false, nil
	}
	if entry.Size >= 0 && info.Size() != entry.Size {
		return true, false, nil
	}
	if entry.MD5 != "" {
		localSum, hashErr := fileMD5(localPath)
		if hashErr != nil {
			return true, false, nil
		}
		return localSum != entry.MD5, false, nil
	}
	if entry.Size >= 0 {
		// Size matched and no MD5 was supplied in manifest.
		return false, false, nil
	}
	// Neither hash nor size was provided in manifest; re-fetch to stay current.
	return true, false, nil
}

func fileMD5(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func downloadFileWithRetry(ctx context.Context, client *http.Client, remoteURL, localPath string, entry RepoEntry) error {
	backoffs := []time.Duration{0, 200 * time.Millisecond, 500 * time.Millisecond}
	var lastErr error

	for attempt, delay := range backoffs {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.New("sync canceled")
			case <-timer.C:
			}
		}

		retryable, err := downloadFileAtomic(ctx, client, remoteURL, localPath, entry)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt == len(backoffs)-1 {
			break
		}
	}
	return lastErr
}

func downloadFileAtomic(ctx context.Context, client *http.Client, remoteURL, localPath string, entry RepoEntry) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return false, fmt.Errorf("cannot build request for %s: %w", entry.RelPath, err)
	}
	req.Header.Set("User-Agent", httpupUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return true, sanitizeHTTPError("downloading "+entry.RelPath, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return true, fmt.Errorf("remote returned HTTP %d for %s", resp.StatusCode, entry.RelPath)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("remote returned HTTP %d for %s", resp.StatusCode, entry.RelPath)
	}

	parentDir := filepath.Dir(localPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return false, fmt.Errorf("cannot create directory for %s: %w", entry.RelPath, err)
	}

	// If localPath is currently a directory, clear it before renaming file into place.
	if info, lerr := os.Lstat(localPath); lerr == nil && info.IsDir() {
		if rmErr := os.RemoveAll(localPath); rmErr != nil {
			return false, fmt.Errorf("cannot replace directory %s with file: %w", entry.RelPath, rmErr)
		}
	}

	tmpFile, err := os.CreateTemp(parentDir, ".ports-tmp-*")
	if err != nil {
		return false, fmt.Errorf("cannot create temp file for %s: %w", entry.RelPath, err)
	}
	tmpName := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmpFile.Close()
			_ = os.Remove(tmpName)
		}
	}()

	hasher := md5.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, hasher), resp.Body); err != nil {
		return true, fmt.Errorf("transfer interrupted for %s: %w", entry.RelPath, err)
	}

	if entry.MD5 != "" {
		gotMD5 := hex.EncodeToString(hasher.Sum(nil))
		if gotMD5 != entry.MD5 {
			return true, fmt.Errorf("checksum mismatch for %s (expected %s, got %s)", entry.RelPath, entry.MD5, gotMD5)
		}
	}

	mode := entry.Mode
	if mode == 0 {
		mode = 0o644
	}
	if err := tmpFile.Chmod(mode.Perm()); err != nil {
		return false, fmt.Errorf("cannot set mode on %s: %w", entry.RelPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		return false, fmt.Errorf("cannot close %s: %w", entry.RelPath, err)
	}

	if err := os.Rename(tmpName, localPath); err != nil {
		return false, fmt.Errorf("cannot finalize %s: %w", entry.RelPath, err)
	}
	cleanup = false
	return false, nil
}

func pruneLocalTree(destDir string, expectedFiles, expectedDirs map[string]struct{}) (int64, error) {
	var pruned int64
	var staleDirs []string

	err := filepath.WalkDir(destDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == destDir {
			return nil
		}
		relFS, err := filepath.Rel(destDir, p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(relFS)

		if d.IsDir() {
			if _, keep := expectedDirs[rel]; !keep {
				staleDirs = append(staleDirs, p)
			}
			return nil
		}

		if _, keep := expectedFiles[rel]; !keep {
			if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("cannot prune removed file %s: %w", rel, rmErr)
			}
			pruned++
		}
		return nil
	})
	if err != nil {
		return pruned, err
	}

	// Remove stale directories deepest-first so nested directories empty cleanly.
	sort.Slice(staleDirs, func(i, j int) bool {
		return len(staleDirs[i]) > len(staleDirs[j])
	})
	for _, dirPath := range staleDirs {
		if rmErr := os.Remove(dirPath); rmErr == nil {
			pruned++
		}
	}

	return pruned, nil
}

func sanitizeHTTPError(action string, err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("sync canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out while %s", action)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Errorf("DNS resolution failed while %s: check /etc/resolv.conf or network link", action)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("network timeout while %s: check link or remote server", action)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("HTTP error while %s: %v", action, urlErr.Err)
	}
	return fmt.Errorf("failed while %s: %v", action, err)
}
