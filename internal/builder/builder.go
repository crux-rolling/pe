// Package builder executes CRUX Pkgfile recipes adapted from pkgutils/pkgmk,
// driven by kpm's manual INI configuration.
package builder

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/config"
	"github.com/kryx-linux/kpm/internal/ports"
	"github.com/kryx-linux/kpm/internal/ui"
)

// Options customizes a single Pkgfile build invocation.
type Options struct {
	Force           bool
	IgnoreFootprint bool
	IgnoreNew       bool
	UpdateFootprint bool
	IgnoreMD5Sum    bool
	IgnoreSignature bool
	KeepWork        bool
	NoStrip         bool
	PublicKey       string
	Out             io.Writer
	ErrOut          io.Writer
}

// Result describes the output artifact of a successful build.
type Result struct {
	Port        ports.Port
	ArchivePath string
	UpToDate    bool
}

// Build builds a CRUX port using its Pkgfile and settings from cfg.
func Build(cfg config.Config, port ports.Port, opts Options) (Result, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.ErrOut == nil {
		opts.ErrOut = io.Discard
	}

	// Always evaluate Pkgfile freshly with bash before building.
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return Result{}, exitErr(ExitPkgfile, err)
	}
	port = evaluated

	if err := os.MkdirAll(cfg.Paths.SourceCache, 0o755); err != nil {
		return Result{}, exitErr(ExitDirPerm, fmt.Errorf("create source_cache %q: %w", cfg.Paths.SourceCache, err))
	}
	if err := os.MkdirAll(cfg.Paths.PackageDir, 0o755); err != nil {
		return Result{}, exitErr(ExitDirPerm, fmt.Errorf("create package_dir %q: %w", cfg.Paths.PackageDir, err))
	}
	if err := os.MkdirAll(cfg.Paths.WorkDir, 0o755); err != nil {
		return Result{}, exitErr(ExitDirPerm, fmt.Errorf("create work_dir %q: %w", cfg.Paths.WorkDir, err))
	}

	targetName := archive.FormatPkgFilename(port.Name, port.Version, port.Release, cfg.Build.Compression)
	targetPath := filepath.Join(cfg.Paths.PackageDir, targetName)

	if !opts.Force && !opts.UpdateFootprint && !buildNeeded(targetPath, port, cfg.Paths.SourceCache) {
		fmt.Fprintf(opts.Out, "=======> Package '%s' is up to date.\n", targetPath)
		return Result{Port: port, ArchivePath: targetPath, UpToDate: true}, nil
	}

	// Step 1: Download missing remote sources into source_cache.
	localSources, err := resolveAndDownloadSources(cfg, port, opts.Out)
	if err != nil {
		return Result{}, exitErr(ExitDownload, err)
	}

	// Step 2: Prepare work directory ($SRC and $PKG).
	workDir := filepath.Join(cfg.Paths.WorkDir, port.Name)
	srcDir := filepath.Join(workDir, "src")
	pkgDir := filepath.Join(workDir, "pkg")

	if err := os.RemoveAll(workDir); err != nil {
		return Result{}, exitErr(ExitDirPerm, fmt.Errorf("clean work dir %q: %w", workDir, err))
	}
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return Result{}, exitErr(ExitDirPerm, fmt.Errorf("create SRC dir: %w", err))
	}
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return Result{}, exitErr(ExitDirPerm, fmt.Errorf("create PKG dir: %w", err))
	}

	keepWork := cfg.Build.KeepWork || opts.KeepWork
	defer func() {
		if !keepWork {
			_ = os.RemoveAll(workDir)
		}
	}()

	// Step 3: Verify .signature / .md5sum if enabled.
	ignoreSig := cfg.Build.IgnoreSignature || opts.IgnoreSignature
	ignoreMD5 := cfg.Build.IgnoreMD5Sum || opts.IgnoreMD5Sum
	sigVerified := false
	if !ignoreSig {
		sigPath := filepath.Join(port.Dir, ".signature")
		if _, err := os.Stat(sigPath); err == nil {
			if err := verifySignature(port, srcDir, localSources, opts.PublicKey); err != nil {
				return Result{}, exitErr(ExitSignature, err)
			}
			sigVerified = true
		}
	}
	if !ignoreMD5 && !sigVerified {
		if err := verifyMD5Sum(port, localSources, opts.ErrOut); err != nil {
			return Result{}, exitErr(ExitMD5, err)
		}
	}

	fmt.Fprintf(opts.Out, "=======> Building '%s'.\n", targetPath)

	// Step 4: Unpack sources and run Pkgfile build() inside bash (matching pkgmk.in).
	if err := runPkgfileBuild(cfg, port, localSources, workDir, srcDir, pkgDir, opts.Out, opts.ErrOut); err != nil {
		return Result{}, exitErr(ExitBuild, fmt.Errorf("building '%s' failed: %w", targetPath, err))
	}

	// Step 5: Strip binaries and compress manpages (matching pkgmk.in).
	if cfg.Build.Strip && !opts.NoStrip {
		if err := stripFiles(port.Dir, pkgDir, cfg.Build.Jobs); err != nil {
			fmt.Fprintf(opts.ErrOut, "=======> WARNING: strip step reported error: %v\n", err)
		}
	}
	if cfg.Build.CompressManpages {
		if err := compressManpages(pkgDir); err != nil {
			fmt.Fprintf(opts.ErrOut, "=======> WARNING: manpage compression reported error: %v\n", err)
		}
	}

	// Step 6: Package $PKG into CRUX .pkg.tar.<compression>.
	if err := archive.Create(pkgDir, targetPath, cfg.Build.Compression); err != nil {
		return Result{}, exitErr(ExitBuild, fmt.Errorf("building '%s' failed: %w", targetPath, err))
	}

	// Step 7: Check or update .footprint.
	ignoreFP := cfg.Build.IgnoreFootprint || opts.IgnoreFootprint
	ignoreNew := cfg.Build.IgnoreNew || opts.IgnoreNew
	if opts.UpdateFootprint {
		if err := updateFootprint(port.Dir, targetPath); err != nil {
			return Result{}, exitErr(ExitFootprint, err)
		}
		fmt.Fprintf(opts.Out, "=======> Footprint updated.\n")
	} else if ignoreFP {
		fmt.Fprintf(opts.ErrOut, "=======> WARNING: Footprint ignored.\n")
	} else {
		if err := checkFootprint(port.Dir, targetPath, ignoreNew, opts.Out, opts.ErrOut); err != nil {
			return Result{}, exitErr(ExitFootprint, fmt.Errorf("building '%s' failed: %w", targetPath, err))
		}
	}

	fmt.Fprintf(opts.Out, "=======> Building '%s' succeeded.\n", targetPath)
	return Result{Port: port, ArchivePath: targetPath, UpToDate: false}, nil
}

func buildNeeded(targetPath string, port ports.Port, sourceCache string) bool {
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return true
	}
	pkgfileInfo, err := os.Stat(filepath.Join(port.Dir, "Pkgfile"))
	if err == nil && !targetInfo.ModTime().After(pkgfileInfo.ModTime()) {
		return true
	}
	for i, src := range port.Source {
		localPath := resolveLocalSourcePath(src, renameAt(port.Renames, i), port.Dir, sourceCache)
		if info, err := os.Stat(localPath); err == nil {
			if !targetInfo.ModTime().After(info.ModTime()) {
				return true
			}
		}
	}
	return false
}

func isRemoteSource(src string) bool {
	return strings.HasPrefix(src, "http://") ||
		strings.HasPrefix(src, "https://") ||
		strings.HasPrefix(src, "ftp://") ||
		strings.HasPrefix(src, "file://")
}

func sourceBasename(src string) string {
	if isRemoteSource(src) {
		if u, err := url.Parse(src); err == nil && u.Path != "" {
			return filepath.Base(u.Path)
		}
	}
	return filepath.Base(src)
}

func renameAt(renames []string, idx int) string {
	if idx >= 0 && idx < len(renames) {
		return renames[idx]
	}
	return ""
}

func resolveLocalSourcePath(src, rename, portDir, sourceCache string) string {
	if rename != "" && rename != "SKIP" {
		return filepath.Join(sourceCache, rename)
	}
	if isRemoteSource(src) {
		return filepath.Join(sourceCache, sourceBasename(src))
	}
	if filepath.IsAbs(src) {
		return src
	}
	return filepath.Join(portDir, src)
}

func resolveAndDownloadSources(cfg config.Config, port ports.Port, out io.Writer) ([]string, error) {
	resolved := make([]string, 0, len(port.Source))
	for i, src := range port.Source {
		rename := renameAt(port.Renames, i)
		localPath := resolveLocalSourcePath(src, rename, port.Dir, cfg.Paths.SourceCache)
		needDownload := false
		if fi, err := os.Stat(localPath); err != nil {
			needDownload = true
		} else if isRemoteSource(src) && looksLikeArchivePath(localPath) {
			if fi.Size() == 0 || fileLooksLikeHTML(localPath) {
				fmt.Fprintf(out, "=======> Removing invalid cached source '%s' and re-downloading.\n", localPath)
				_ = os.Remove(localPath)
				needDownload = true
			}
		}
		if needDownload {
			if !isRemoteSource(src) {
				return nil, fmt.Errorf("source file %q not found (not a URL)", localPath)
			}
			if !cfg.Build.DownloadSources {
				return nil, fmt.Errorf("source file %q not found (download_sources is disabled in kpm.ini)", localPath)
			}
			if err := downloadSourceFile(cfg, src, localPath, out); err != nil {
				return nil, err
			}
		}
		resolved = append(resolved, localPath)
	}
	return resolved, nil
}

const downloadUserAgent = "curl/8.12.1"

var downloadRetryDelay = time.Second

func looksLikeArchivePath(p string) bool {
	low := strings.ToLower(p)
	for _, ext := range []string{
		".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2",
		".tar.xz", ".txz", ".tar.lz", ".tar.lzma", ".tar.zst", ".tzst",
		".zip", ".gz", ".bz2", ".xz", ".lz", ".zst", ".7z", ".rpm", ".deb",
	} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

func looksLikeHTMLBytes(buf []byte) bool {
	trimmed := bytes.TrimSpace(buf)
	if len(trimmed) == 0 {
		return false
	}
	low := strings.ToLower(string(trimmed))
	return strings.HasPrefix(low, "<!doctype html") ||
		strings.HasPrefix(low, "<html") ||
		strings.HasPrefix(low, "<head") ||
		strings.HasPrefix(low, "<title") ||
		(strings.HasPrefix(low, "<!--") && strings.Contains(low, "<html"))
}

func fileLooksLikeHTML(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return looksLikeHTMLBytes(buf[:n])
}

func fallbackSourceURLs(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	cleanPath := strings.TrimPrefix(u.Path, "/")
	var out []string
	switch host {
	case "ftpmirror.gnu.org":
		gnuPath := strings.TrimPrefix(cleanPath, "gnu/")
		if gnuPath != "" {
			out = append(out,
				"https://ftp.gnu.org/gnu/"+gnuPath,
				"https://mirrors.kernel.org/gnu/"+gnuPath,
			)
		}
	case "ftp.gnu.org":
		gnuPath := strings.TrimPrefix(cleanPath, "gnu/")
		if gnuPath != "" {
			out = append(out, "https://mirrors.kernel.org/gnu/"+gnuPath)
		}
	case "gmplib.org", "www.gmplib.org":
		if strings.HasPrefix(cleanPath, "download/gmp/") {
			rel := strings.TrimPrefix(cleanPath, "download/gmp/")
			if rel != "" {
				out = append(out,
					"https://ftp.gnu.org/gnu/gmp/"+rel,
					"https://mirrors.kernel.org/gnu/gmp/"+rel,
				)
			}
		}
	case "sourceforge.net", "www.sourceforge.net":
		// Rewrite /projects/<proj>/files/<rest>[/download] -> downloads.sourceforge.net/project/<proj>/<rest>
		parts := strings.Split(cleanPath, "/")
		if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "files" {
			proj := parts[1]
			restParts := parts[3:]
			if len(restParts) > 0 && restParts[len(restParts)-1] == "download" {
				restParts = restParts[:len(restParts)-1]
			}
			rest := strings.Join(restParts, "/")
			if proj != "" && rest != "" {
				out = append(out, "https://downloads.sourceforge.net/project/"+proj+"/"+rest)
				if proj == "freetype" {
					base := filepath.Base(rest)
					out = append(out,
						"https://download.savannah.gnu.org/releases/freetype/"+base,
						"https://download-mirror.savannah.gnu.org/releases/freetype/"+base,
					)
				}
			}
		}
	case "download.savannah.gnu.org", "download-mirror.savannah.gnu.org",
		"download.savannah.nongnu.org", "download-mirror.savannah.nongnu.org":
		if cleanPath != "" {
			for _, alt := range []string{
				"download.savannah.gnu.org",
				"download-mirror.savannah.gnu.org",
				"download.savannah.nongnu.org",
				"download-mirror.savannah.nongnu.org",
			} {
				if alt != host {
					out = append(out, "https://"+alt+"/"+cleanPath)
				}
			}
		}
	case "www.kernel.org", "kernel.org":
		if strings.HasPrefix(cleanPath, "pub/") {
			out = append(out, "https://mirrors.edge.kernel.org/"+cleanPath)
		}
	}
	return out
}

func isRedirectorMirrorHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "ftpmirror.gnu.org", "sourceforge.net", "www.sourceforge.net",
		"downloads.sourceforge.net", "prdownloads.sourceforge.net", "dl.sourceforge.net":
		return true
	default:
		return false
	}
}

func downloadSourceFile(cfg config.Config, rawURL, destPath string, out io.Writer) error {
	fmt.Fprintf(out, "=======> Downloading '%s'.\n", rawURL)
	base := sourceBasename(rawURL)

	fallbacks := fallbackSourceURLs(rawURL)
	candidates := make([]string, 0, len(cfg.Build.SourceMirrors)+1+len(fallbacks))
	seen := make(map[string]bool)
	addCandidate := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			candidates = append(candidates, u)
		}
	}
	for _, mirror := range cfg.Build.SourceMirrors {
		m := strings.TrimRight(mirror, "/")
		if m != "" {
			addCandidate(m + "/" + base)
		}
	}
	addCandidate(rawURL)
	for _, fb := range fallbacks {
		addCandidate(fb)
	}

	var lastErr error
	var httpCandidates []string
	for idx, candidate := range candidates {
		if idx > 0 && candidate != rawURL {
			fmt.Fprintf(out, "=======> Trying fallback mirror '%s'.\n", candidate)
		}
		if strings.HasPrefix(candidate, "file://") {
			u, err := url.Parse(candidate)
			if err != nil {
				lastErr = err
				continue
			}
			if err := copyFile(u.Path, destPath); err == nil {
				return nil
			} else {
				lastErr = err
				continue
			}
		}
		if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
			httpCandidates = append(httpCandidates, candidate)
			if err := httpDownloadWithRetry(candidate, destPath, time.Duration(cfg.Repo.TimeoutSeconds)*time.Second, 3, out, ui.NewStyler(cfg.UI.Color, out)); err == nil {
				return nil
			} else {
				lastErr = err
				continue
			}
		}
		if err := externalDownload(cfg, candidate, destPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}

	// Final fallback: if pure-Go HTTP failed (e.g. strict TLS fingerprinting / HTTP/2
	// bot filter on an upstream host), retry via external curl/wget if available.
	if len(httpCandidates) > 0 {
		prog := cfg.Build.DownloadProg
		if prog == "" {
			prog = "curl"
		}
		if _, err := exec.LookPath(prog); err == nil {
			for _, candidate := range httpCandidates {
				fmt.Fprintf(out, "=======> Retrying with %s: '%s'.\n", prog, candidate)
				if derr := externalDownload(cfg, candidate, destPath); derr == nil {
					return nil
				} else {
					lastErr = derr
				}
			}
		}
	}
	return fmt.Errorf("downloading %q failed: %w", rawURL, lastErr)
}

func externalDownload(cfg config.Config, candidate, destPath string) error {
	prog := cfg.Build.DownloadProg
	if prog == "" {
		prog = "curl"
	}
	partial := destPath + ".partial"
	_ = os.Remove(partial)
	var cmd *exec.Cmd
	switch prog {
	case "wget":
		args := []string{"--tries=3", "--user-agent=" + downloadUserAgent, "--output-document", partial}
		if cfg.Build.WgetOpts != "" {
			args = append(args, strings.Fields(cfg.Build.WgetOpts)...)
		}
		args = append(args, candidate)
		cmd = exec.Command("wget", args...)
	default:
		args := []string{"-fsSL", "--retry", "2", "-A", downloadUserAgent, "-o", partial}
		if cfg.Build.CurlOpts != "" {
			args = append(args, strings.Fields(cfg.Build.CurlOpts)...)
		}
		args = append(args, candidate)
		cmd = exec.Command("curl", args...)
	}
	if err := cmd.Run(); err != nil {
		_ = os.Remove(partial)
		return err
	}
	fi, err := os.Stat(partial)
	if err != nil || fi.Size() == 0 {
		_ = os.Remove(partial)
		return fmt.Errorf("%s downloaded empty file for %s", prog, candidate)
	}
	if looksLikeArchivePath(destPath) && fileLooksLikeHTML(partial) {
		_ = os.Remove(partial)
		return fmt.Errorf("server returned HTML page instead of archive for %s", candidate)
	}
	if err := os.Rename(partial, destPath); err != nil {
		_ = os.Remove(partial)
		return err
	}
	return nil
}

type httpStatusError struct {
	code   int
	status string
}

func (e httpStatusError) Error() string {
	return "HTTP " + e.status
}

func httpDownloadWithRetry(rawURL, destPath string, timeout time.Duration, maxAttempts int, out io.Writer, styler ui.Styler) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := httpDownload(rawURL, destPath, timeout, out, styler)
		if err == nil {
			return nil
		}
		lastErr = err
		// Don't waste retries on deterministic 4xx client errors unless the host is
		// a random mirror redirector (where a different mirror may have the file).
		if st, ok := err.(httpStatusError); ok && st.code >= 400 && st.code < 500 && st.code != http.StatusTooManyRequests && !isRedirectorMirrorHost(rawURL) {
			return err
		}
		if attempt < maxAttempts {
			fmt.Fprintf(out, "=======> Download attempt %d/%d failed (%v), retrying...\n", attempt, maxAttempts, err)
			if downloadRetryDelay > 0 {
				time.Sleep(downloadRetryDelay * time.Duration(attempt))
			}
		}
	}
	return lastErr
}

func httpDownload(rawURL, destPath string, timeout time.Duration, out io.Writer, styler ui.Styler) error {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	client := &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			req.Header.Set("User-Agent", downloadUserAgent)
			req.Header.Set("Accept", "*/*")
			return nil
		},
	}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", downloadUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError{code: resp.StatusCode, status: resp.Status}
	}
	expectArchive := looksLikeArchivePath(destPath)
	if expectArchive {
		ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
		if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml+xml") {
			return fmt.Errorf("server returned HTML (%s) instead of archive for %s", ct, rawURL)
		}
	}

	// Peek initial bytes before creating the final file so HTML interstitials never poison source_cache.
	sniffBuf := make([]byte, 512)
	sniffN, sniffErr := io.ReadFull(resp.Body, sniffBuf)
	if sniffErr != nil && sniffErr != io.EOF && sniffErr != io.ErrUnexpectedEOF {
		return sniffErr
	}
	sniff := sniffBuf[:sniffN]
	if len(sniff) == 0 {
		return fmt.Errorf("empty response body from %s", rawURL)
	}
	if expectArchive && looksLikeHTMLBytes(sniff) {
		return fmt.Errorf("server returned HTML page instead of archive for %s", rawURL)
	}

	prog := ui.NewProgress(out, styler, "downloading "+filepath.Base(destPath), resp.ContentLength)
	partial := destPath + ".partial"
	f, err := os.Create(partial)
	if err != nil {
		prog.Fail()
		return err
	}
	reader := io.MultiReader(bytes.NewReader(sniff), resp.Body)
	if _, err := io.Copy(f, io.TeeReader(reader, prog)); err != nil {
		prog.Fail()
		_ = f.Close()
		_ = os.Remove(partial)
		return err
	}
	_ = f.Chmod(0o644)
	if err := f.Close(); err != nil {
		prog.Fail()
		_ = os.Remove(partial)
		return err
	}
	if err := os.Rename(partial, destPath); err != nil {
		prog.Fail()
		return err
	}
	prog.Finish("")
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func verifySignature(port ports.Port, srcDir string, localSources []string, publicKey string) error {
	signifyPath, err := exec.LookPath("signify")
	if err != nil {
		return fmt.Errorf("signify not found in PATH (use --ignore-signature to skip)")
	}
	sigPath := filepath.Join(port.Dir, ".signature")
	links := []string{
		filepath.Join(port.Dir, "Pkgfile"),
		filepath.Join(port.Dir, ".footprint"),
	}
	links = append(links, localSources...)
	var created []string
	for _, target := range links {
		if _, err := os.Stat(target); err != nil {
			continue
		}
		link := filepath.Join(srcDir, filepath.Base(target))
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
		created = append(created, link)
	}
	defer func() {
		for _, l := range created {
			_ = os.Remove(l)
		}
	}()

	args := []string{"-q", "-C"}
	if strings.TrimSpace(publicKey) != "" {
		args = append(args, "-p", publicKey)
	} else if port.Collection != "" {
		cand := filepath.Join("/etc/ports", port.Collection+".pub")
		if _, err := os.Stat(cand); err == nil {
			args = append(args, "-p", cand)
		}
	}
	args = append(args, "-x", sigPath)
	cmd := exec.Command(signifyPath, args...)
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("signature verification failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func verifyMD5Sum(port ports.Port, localSources []string, errOut io.Writer) error {
	md5Path := filepath.Join(port.Dir, ".md5sum")
	if len(localSources) == 0 {
		return nil
	}
	computed, err := computeMD5Lines(localSources)
	if err != nil {
		return err
	}
	if _, err := os.Stat(md5Path); os.IsNotExist(err) {
		fmt.Fprintf(errOut, "=======> WARNING: Md5sum not found, skipping verification.\n")
		return nil
	}
	raw, err := os.ReadFile(md5Path)
	if err != nil {
		return fmt.Errorf("read %s: %w", md5Path, err)
	}
	expected := normalizeLines(string(raw))
	sort.Slice(expected, func(i, j int) bool { return md5Key(expected[i]) < md5Key(expected[j]) })

	if !equalLines(expected, computed) {
		return fmt.Errorf("md5sum mismatch for %s (expected %v, got %v)", port.Name, expected, computed)
	}
	return nil
}

func computeMD5Lines(paths []string) ([]string, error) {
	lines := make([]string, 0, len(paths))
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("open source %q for md5sum: %w", p, err)
		}
		h := md5.New()
		_, copyErr := io.Copy(h, f)
		_ = f.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("hash source %q: %w", p, copyErr)
		}
		lines = append(lines, fmt.Sprintf("%s  %s", hex.EncodeToString(h.Sum(nil)), filepath.Base(p)))
	}
	sort.Slice(lines, func(i, j int) bool { return md5Key(lines[i]) < md5Key(lines[j]) })
	return lines, nil
}

func md5Key(line string) string {
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		return fields[1]
	}
	return line
}

func buildUnpackFunction(localSources []string) string {
	var sb strings.Builder
	sb.WriteString("unpack_source() {\n")
	sb.WriteString("  :\n")
	for _, src := range localSources {
		q := shellQuote(src)
		sb.WriteString("  case " + q + " in\n")
		sb.WriteString("    *.tar|*.tar.gz|*.tar.Z|*.tgz|*.tar.bz2|*.tbz2|*.tar.xz|*.txz|*.tar.lzma|*.tar.lz|*.tar.zst|*.tzst|*.zip|*.rpm|*.7z)\n")
		sb.WriteString("      bsdtar -p -o -C \"$SRC\" -xf " + q + " ;;\n")
		sb.WriteString("    *)\n")
		sb.WriteString("      cp -a " + q + " \"$SRC\" ;;\n")
		sb.WriteString("  esac\n")
	}
	sb.WriteString("}\n")
	return sb.String()
}

func buildPkgfileEnv(cfg config.Config, port ports.Port, workDir, srcDir, pkgDir string) []string {
	pkgmkArch := "64"
	if _, err := os.Stat(filepath.Join(port.Dir, ".32bit")); err == nil {
		pkgmkArch = "32"
	}
	env := append(os.Environ(),
		"SRC="+srcDir,
		"PKG="+pkgDir,
		"PKGMK_ROOT="+port.Dir,
		"PKGMK_ARCH="+pkgmkArch,
		"PKGMK_SOURCE_DIR="+cfg.Paths.SourceCache,
		"PKGMK_PACKAGE_DIR="+cfg.Paths.PackageDir,
		"PKGMK_WORK_DIR="+workDir,
		"JOBS="+strconv.Itoa(cfg.Build.Jobs),
		"CFLAGS="+cfg.Build.CFLAGS,
		"CXXFLAGS="+cfg.Build.CXXFLAGS,
		"MAKEFLAGS="+cfg.Build.MAKEFLAGS,
	)
	if strings.TrimSpace(cfg.Paths.PortsDir) != "" {
		env = append(env,
			"KRYX_PORTS_DIR="+cfg.Paths.PortsDir,
			"KPM_PORTS_DIR="+cfg.Paths.PortsDir,
		)
	}
	if cfg.Build.LDFLAGS != "" {
		env = append(env, "LDFLAGS="+cfg.Build.LDFLAGS)
	}
	return env
}

func runPkgfileBuild(cfg config.Config, port ports.Port, localSources []string, workDir, srcDir, pkgDir string, out, errOut io.Writer) error {
	// Default unpack_source matches pkgmk.in, and Pkgfile can override unpack_source() before build() is called.
	var sb strings.Builder
	sb.WriteString("set -e\n")
	sb.WriteString("umask 022\n")
	sb.WriteString("export LC_ALL=C.UTF-8\n")
	sb.WriteString(buildUnpackFunction(localSources))
	sb.WriteString("source " + shellQuote(filepath.Join(port.Dir, "Pkgfile")) + "\n")
	sb.WriteString("unpack_source\n")
	sb.WriteString("cd \"$SRC\"\n")
	sb.WriteString("(set -e; build)\n")

	cmd := exec.Command("bash", "-c", sb.String())
	cmd.Dir = port.Dir
	cmd.Stdout = out
	cmd.Stderr = errOut
	cmd.Env = buildPkgfileEnv(cfg, port, workDir, srcDir, pkgDir)
	return cmd.Run()
}

func stripFiles(portDir, pkgDir string, jobs int) error {
	if _, err := exec.LookPath("strip"); err != nil {
		return nil
	}
	if _, err := exec.LookPath("file"); err != nil {
		return nil
	}
	if jobs < 1 {
		jobs = 1
	}
	nostrip := filepath.Join(portDir, ".nostrip")
	filter := "cat"
	if _, err := os.Stat(nostrip); err == nil {
		filter = "grep -v -f " + shellQuote(nostrip)
	}
	script := fmt.Sprintf(`
cd %s
find . -type f -printf '%%P\n' | %s \
  | xargs -r -L10 -P%d file --no-buffer --separator '>' -e apptype -e ascii -e encoding -e tokens -e cdf -e compress -e tar -e text \
  | awk 'BEGIN { FS = ">[ ]+" }
    $0 ~ /ELF.*executable.*not stripped/    { print "--strip-all \"" $1 "\"" }
    $0 ~ /ELF.*shared object.*not stripped/ { print "--strip-unneeded \"" $1 "\"" }
    $2 == "current ar archive"              { print "--strip-debug \"" $1 "\"" }' \
  | xargs -r -L1 -P%d strip
`, shellQuote(pkgDir), filter, jobs, jobs)
	cmd := exec.Command("bash", "-c", script)
	return cmd.Run()
}

func compressManpages(pkgDir string) error {
	if _, err := exec.LookPath("gzip"); err != nil {
		return nil
	}
	script := fmt.Sprintf(`
cd %s
find . -type f -path "*/man/man*/*" | while IFS= read -r FILE; do
	if [ "$FILE" = "${FILE%%.gz}" ]; then
		gzip -9 "$FILE"
	fi
done
find . -type l -path "*/man/man*/*" | while IFS= read -r FILE; do
	TARGET=$(readlink -n "$FILE")
	TARGET="${TARGET##*/}"
	TARGET="${TARGET%%.gz}.gz"
	rm -f "$FILE"
	FILE="${FILE%%.gz}.gz"
	DIR=$(dirname "$FILE")
	if [ -e "$DIR/$TARGET" ]; then
		ln -sf "$TARGET" "$FILE"
	fi
done
`, shellQuote(pkgDir))
	cmd := exec.Command("bash", "-c", script)
	return cmd.Run()
}

func generateSortedFootprint(targetPath string) ([]string, error) {
	var buf bytes.Buffer
	if err := archive.Footprint(targetPath, &buf); err != nil {
		return nil, err
	}
	kver := ""
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		kver = strings.TrimSpace(string(out))
	}
	var lines []string
	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if kver != "" {
			line = strings.ReplaceAll(line, "\tlib/modules/"+kver+"/", "\tlib/modules/<kernel-version>/")
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	sort.Slice(lines, func(i, j int) bool {
		return footprintSortKey(lines[i]) < footprintSortKey(lines[j])
	})
	return lines, nil
}

func updateFootprint(portDir, targetPath string) error {
	lines, err := generateSortedFootprint(targetPath)
	if err != nil {
		return err
	}
	fpPath := filepath.Join(portDir, ".footprint")
	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(fpPath, []byte(content), 0o644)
}

func checkFootprint(portDir, targetPath string, ignoreNew bool, out, errOut io.Writer) error {
	actual, err := generateSortedFootprint(targetPath)
	if err != nil {
		return err
	}
	fpPath := filepath.Join(portDir, ".footprint")
	if _, err := os.Stat(fpPath); os.IsNotExist(err) {
		fmt.Fprintf(errOut, "=======> WARNING: Footprint not found, creating new.\n")
		return os.WriteFile(fpPath, []byte(strings.Join(actual, "\n")+"\n"), 0o644)
	}
	raw, err := os.ReadFile(fpPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", fpPath, err)
	}
	expected := normalizeLines(string(raw))
	sort.Slice(expected, func(i, j int) bool {
		return footprintSortKey(expected[i]) < footprintSortKey(expected[j])
	})

	expSet := make(map[string]bool, len(expected))
	actSet := make(map[string]bool, len(actual))
	for _, l := range expected {
		expSet[l] = true
	}
	for _, l := range actual {
		actSet[l] = true
	}

	var missingLines, newLines []string
	for _, l := range expected {
		if !actSet[l] {
			missingLines = append(missingLines, l)
		}
	}
	for _, l := range actual {
		if !expSet[l] {
			newLines = append(newLines, l)
		}
	}

	if len(missingLines) == 0 && len(newLines) == 0 {
		return nil
	}

	if ignoreNew && len(missingLines) == 0 {
		fmt.Fprintf(out, "=======> New files found:\n")
		for _, l := range newLines {
			fmt.Fprintf(errOut, "NEW       %s\n", l)
		}
		return nil
	}

	fmt.Fprintf(errOut, "=======> ERROR: Footprint mismatch found:\n")
	for _, l := range missingLines {
		fmt.Fprintf(errOut, "MISSING   %s\n", l)
	}
	for _, l := range newLines {
		fmt.Fprintf(errOut, "NEW       %s\n", l)
	}
	return fmt.Errorf("footprint mismatch (%d missing, %d new)", len(missingLines), len(newLines))
}

func footprintSortKey(line string) string {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return line
}

func normalizeLines(raw string) []string {
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
