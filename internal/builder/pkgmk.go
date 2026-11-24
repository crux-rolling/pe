package builder

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kryx-linux/kpm/internal/archive"
	"github.com/kryx-linux/kpm/internal/config"
	"github.com/kryx-linux/kpm/internal/ports"
)

// Upstream CRUX pkgmk exit codes (from pkgutils/pkgmk.in).
const (
	ExitGeneral   = 1
	ExitPkgfile   = 2
	ExitDirPerm   = 3
	ExitDownload  = 4
	ExitUnpack    = 5
	ExitMD5       = 6
	ExitFootprint = 7
	ExitBuild     = 8
	ExitInstall   = 9
	ExitSignature = 10
)

// ExitError wraps a build error with the exact upstream CRUX pkgmk exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ExitCodeOf returns the upstream pkgmk exit code for err (default 1, or 0 for nil).
func ExitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code > 0 {
		return ee.Code
	}
	return ExitGeneral
}

func exitErr(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Err: err}
}

// LoadPkgmkConf sources a CRUX /etc/pkgmk.conf (or -cf <file>) via bash and applies
// all PKGMK_* and compiler/build variables into cfg.
func LoadPkgmkConf(path string, cfg *config.Config) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	script := `set -e
source ` + shellQuote(path) + `
printf 'CFLAGS=%s\n' "${CFLAGS-}"
printf 'CXXFLAGS=%s\n' "${CXXFLAGS-}"
printf 'LDFLAGS=%s\n' "${LDFLAGS-}"
printf 'MAKEFLAGS=%s\n' "${MAKEFLAGS-}"
printf 'JOBS=%s\n' "${JOBS-}"
printf 'PKGMK_SOURCE_DIR=%s\n' "${PKGMK_SOURCE_DIR-}"
printf 'PKGMK_PACKAGE_DIR=%s\n' "${PKGMK_PACKAGE_DIR-}"
printf 'PKGMK_WORK_DIR=%s\n' "${PKGMK_WORK_DIR-}"
printf 'PKGMK_COMPRESSION_MODE=%s\n' "${PKGMK_COMPRESSION_MODE-}"
printf 'PKGMK_DOWNLOAD=%s\n' "${PKGMK_DOWNLOAD-}"
printf 'PKGMK_DOWNLOAD_PROG=%s\n' "${PKGMK_DOWNLOAD_PROG-}"
printf 'PKGMK_CURL_OPTS=%s\n' "${PKGMK_CURL_OPTS-}"
printf 'PKGMK_WGET_OPTS=%s\n' "${PKGMK_WGET_OPTS-}"
printf 'PKGMK_IGNORE_FOOTPRINT=%s\n' "${PKGMK_IGNORE_FOOTPRINT-}"
printf 'PKGMK_IGNORE_NEW=%s\n' "${PKGMK_IGNORE_NEW-}"
printf 'PKGMK_IGNORE_MD5SUM=%s\n' "${PKGMK_IGNORE_MD5SUM-}"
printf 'PKGMK_IGNORE_SIGNATURE=%s\n' "${PKGMK_IGNORE_SIGNATURE-}"
printf 'PKGMK_NO_STRIP=%s\n' "${PKGMK_NO_STRIP-}"
printf 'PKGMK_COMPRESS_MANPAGES=%s\n' "${PKGMK_COMPRESS_MANPAGES-}"
printf 'PKGMK_KEEP_WORK=%s\n' "${PKGMK_KEEP_WORK-}"
if [ "${#PKGMK_SOURCE_MIRRORS[@]}" -gt 0 ] 2>/dev/null; then
  printf 'PKGMK_SOURCE_MIRRORS=%s\n' "${PKGMK_SOURCE_MIRRORS[*]}"
fi
`
	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("source %s: %w", path, err)
	}
	vars := parseKVLines(string(out))
	applyPkgmkVars(vars, cfg)
	return nil
}

// ApplyPkgmkEnv overlays any PKGMK_* environment variables from os.Environ onto cfg,
// matching upstream pkgmk's environment override rules.
func ApplyPkgmkEnv(cfg *config.Config) {
	keys := []string{
		"CFLAGS", "CXXFLAGS", "LDFLAGS", "MAKEFLAGS", "JOBS",
		"PKGMK_SOURCE_DIR", "PKGMK_PACKAGE_DIR", "PKGMK_WORK_DIR",
		"PKGMK_COMPRESSION_MODE", "PKGMK_SOURCE_MIRRORS",
		"PKGMK_DOWNLOAD", "PKGMK_DOWNLOAD_PROG", "PKGMK_CURL_OPTS", "PKGMK_WGET_OPTS",
		"PKGMK_IGNORE_FOOTPRINT", "PKGMK_IGNORE_NEW", "PKGMK_IGNORE_MD5SUM",
		"PKGMK_IGNORE_SIGNATURE", "PKGMK_NO_STRIP", "PKGMK_COMPRESS_MANPAGES",
		"PKGMK_KEEP_WORK",
	}
	vars := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			vars[k] = v
		}
	}
	applyPkgmkVars(vars, cfg)
}

func applyPkgmkVars(vars map[string]string, cfg *config.Config) {
	if v := strings.TrimSpace(vars["CFLAGS"]); v != "" {
		cfg.Build.CFLAGS = v
	}
	if v := strings.TrimSpace(vars["CXXFLAGS"]); v != "" {
		cfg.Build.CXXFLAGS = v
	}
	if v := strings.TrimSpace(vars["LDFLAGS"]); v != "" {
		cfg.Build.LDFLAGS = v
	}
	if v := strings.TrimSpace(vars["MAKEFLAGS"]); v != "" {
		cfg.Build.MAKEFLAGS = v
	}
	if v := strings.TrimSpace(vars["JOBS"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Build.Jobs = n
		}
	}
	if v := strings.TrimSpace(vars["PKGMK_SOURCE_DIR"]); v != "" {
		cfg.Paths.SourceCache = v
	}
	if v := strings.TrimSpace(vars["PKGMK_PACKAGE_DIR"]); v != "" {
		cfg.Paths.PackageDir = v
	}
	if v := strings.TrimSpace(vars["PKGMK_WORK_DIR"]); v != "" {
		cfg.Paths.WorkDir = v
	}
	if v := strings.ToLower(strings.TrimSpace(vars["PKGMK_COMPRESSION_MODE"])); v != "" && archive.IsSupportedExt(v) {
		cfg.Build.Compression = v
	}
	if v := strings.TrimSpace(vars["PKGMK_SOURCE_MIRRORS"]); v != "" {
		cfg.Build.SourceMirrors = strings.Fields(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_DOWNLOAD"]); v != "" {
		cfg.Build.DownloadSources = isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_DOWNLOAD_PROG"]); v != "" {
		cfg.Build.DownloadProg = v
	}
	if v := strings.TrimSpace(vars["PKGMK_CURL_OPTS"]); v != "" {
		cfg.Build.CurlOpts = v
	}
	if v := strings.TrimSpace(vars["PKGMK_WGET_OPTS"]); v != "" {
		cfg.Build.WgetOpts = v
	}
	if v := strings.TrimSpace(vars["PKGMK_IGNORE_FOOTPRINT"]); v != "" {
		cfg.Build.IgnoreFootprint = isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_IGNORE_NEW"]); v != "" {
		cfg.Build.IgnoreNew = isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_IGNORE_MD5SUM"]); v != "" {
		cfg.Build.IgnoreMD5Sum = isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_IGNORE_SIGNATURE"]); v != "" {
		cfg.Build.IgnoreSignature = isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_NO_STRIP"]); v != "" {
		cfg.Build.Strip = !isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_COMPRESS_MANPAGES"]); v != "" {
		cfg.Build.CompressManpages = isYes(v)
	}
	if v := strings.TrimSpace(vars["PKGMK_KEEP_WORK"]); v != "" {
		cfg.Build.KeepWork = isYes(v)
	}
}

func isYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "1", "on":
		return true
	default:
		return false
	}
}

func parseKVLines(raw string) map[string]string {
	m := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if eq := strings.IndexByte(line, '='); eq > 0 {
			m[line[:eq]] = line[eq+1:]
		}
	}
	return m
}

// TargetArchivePath computes the expected .pkg.tar.<ext> output path for port.
func TargetArchivePath(cfg config.Config, port ports.Port) string {
	name := archive.FormatPkgFilename(port.Name, port.Version, port.Release, cfg.Build.Compression)
	return filepath.Join(cfg.Paths.PackageDir, name)
}

// UpToDate reports whether port's built archive is up to date relative to Pkgfile and sources
// (matching CRUX `pkgmk -utd`).
func UpToDate(cfg config.Config, port ports.Port) (bool, string, error) {
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return false, "", exitErr(ExitPkgfile, err)
	}
	target := TargetArchivePath(cfg, evaluated)
	return !buildNeeded(target, evaluated, cfg.Paths.SourceCache), target, nil
}

// DownloadOnly downloads missing remote source files for port into SourceCache
// (matching CRUX `pkgmk -do`).
func DownloadOnly(cfg config.Config, port ports.Port, out io.Writer) ([]string, error) {
	if out == nil {
		out = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return nil, exitErr(ExitPkgfile, err)
	}
	if err := os.MkdirAll(cfg.Paths.SourceCache, 0o755); err != nil {
		return nil, exitErr(ExitDirPerm, fmt.Errorf("create source_cache %q: %w", cfg.Paths.SourceCache, err))
	}
	cfg.Build.DownloadSources = true
	sources, err := resolveAndDownloadSources(cfg, evaluated, out)
	if err != nil {
		return nil, exitErr(ExitDownload, err)
	}
	return sources, nil
}

// ExtractOnly downloads/verifies sources and runs unpack_source into workDir/src without building
// (matching CRUX `pkgmk -eo`).
func ExtractOnly(cfg config.Config, port ports.Port, opts Options) (string, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.ErrOut == nil {
		opts.ErrOut = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return "", exitErr(ExitPkgfile, err)
	}
	if err := os.MkdirAll(cfg.Paths.SourceCache, 0o755); err != nil {
		return "", exitErr(ExitDirPerm, err)
	}
	if err := os.MkdirAll(cfg.Paths.WorkDir, 0o755); err != nil {
		return "", exitErr(ExitDirPerm, err)
	}

	localSources, err := resolveAndDownloadSources(cfg, evaluated, opts.Out)
	if err != nil {
		return "", exitErr(ExitDownload, err)
	}

	workDir := filepath.Join(cfg.Paths.WorkDir, evaluated.Name)
	srcDir := filepath.Join(workDir, "src")
	pkgDir := filepath.Join(workDir, "pkg")
	_ = os.RemoveAll(workDir)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return "", exitErr(ExitDirPerm, err)
	}
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return "", exitErr(ExitDirPerm, err)
	}

	ignoreSig := cfg.Build.IgnoreSignature || opts.IgnoreSignature
	ignoreMD5 := cfg.Build.IgnoreMD5Sum || opts.IgnoreMD5Sum
	sigVerified := false
	if !ignoreSig {
		if _, statErr := os.Stat(filepath.Join(evaluated.Dir, ".signature")); statErr == nil {
			if err := verifySignature(evaluated, srcDir, localSources, opts.PublicKey); err != nil {
				return "", exitErr(ExitSignature, err)
			}
			sigVerified = true
		}
	}
	if !ignoreMD5 && !sigVerified {
		if err := verifyMD5Sum(evaluated, localSources, opts.ErrOut); err != nil {
			return "", exitErr(ExitMD5, err)
		}
	}

	var sb strings.Builder
	sb.WriteString("set -e\n")
	sb.WriteString("umask 022\n")
	sb.WriteString("export LC_ALL=C.UTF-8\n")
	sb.WriteString(buildUnpackFunction(localSources))
	sb.WriteString("source " + shellQuote(filepath.Join(evaluated.Dir, "Pkgfile")) + "\n")
	sb.WriteString("unpack_source\n")

	cmd := exec.Command("bash", "-c", sb.String())
	cmd.Dir = evaluated.Dir
	cmd.Stdout = opts.Out
	cmd.Stderr = opts.ErrOut
	cmd.Env = buildPkgfileEnv(cfg, evaluated, workDir, srcDir, pkgDir)
	if err := cmd.Run(); err != nil {
		return "", exitErr(ExitUnpack, fmt.Errorf("unpacking sources failed: %w", err))
	}
	return srcDir, nil
}

// UpdateMD5Sum regenerates <portDir>/.md5sum from the port's source files
// (matching CRUX `pkgmk -um`).
func UpdateMD5Sum(cfg config.Config, port ports.Port, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return exitErr(ExitPkgfile, err)
	}
	localSources, err := resolveAndDownloadSources(cfg, evaluated, out)
	if err != nil {
		return exitErr(ExitDownload, err)
	}
	lines, err := computeMD5Lines(localSources)
	if err != nil {
		return exitErr(ExitMD5, err)
	}
	md5Path := filepath.Join(evaluated.Dir, ".md5sum")
	var content string
	if len(lines) > 0 {
		content = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(md5Path, []byte(content), 0o644); err != nil {
		return exitErr(ExitDirPerm, err)
	}
	fmt.Fprintf(out, "=======> Md5sum updated.\n")
	return nil
}

// CheckMD5Sum verifies <portDir>/.md5sum against source files (matching CRUX `pkgmk -cm`).
func CheckMD5Sum(cfg config.Config, port ports.Port, out, errOut io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return exitErr(ExitPkgfile, err)
	}
	localSources, err := resolveAndDownloadSources(cfg, evaluated, out)
	if err != nil {
		return exitErr(ExitDownload, err)
	}
	if err := verifyMD5Sum(evaluated, localSources, errOut); err != nil {
		return exitErr(ExitMD5, err)
	}
	fmt.Fprintf(out, "=======> Md5sum check succeeded.\n")
	return nil
}

// UpdateFootprintFile regenerates <portDir>/.footprint from an existing built archive
// (matching CRUX `pkgmk -uf`).
func UpdateFootprintFile(cfg config.Config, port ports.Port, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return exitErr(ExitPkgfile, err)
	}
	target := TargetArchivePath(cfg, evaluated)
	if _, err := os.Stat(target); err != nil {
		return exitErr(ExitFootprint, fmt.Errorf("unable to update footprint: package '%s' not found", target))
	}
	if err := updateFootprint(evaluated.Dir, target); err != nil {
		return exitErr(ExitFootprint, err)
	}
	fmt.Fprintf(out, "=======> Footprint updated.\n")
	return nil
}

// CheckSignature verifies <portDir>/.signature (matching CRUX `pkgmk -cs`).
func CheckSignature(cfg config.Config, port ports.Port, publicKey string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return exitErr(ExitPkgfile, err)
	}
	localSources, err := resolveAndDownloadSources(cfg, evaluated, out)
	if err != nil {
		return exitErr(ExitDownload, err)
	}
	tmpDir, err := os.MkdirTemp("", "kpm-sigchk-*")
	if err != nil {
		return exitErr(ExitGeneral, err)
	}
	defer os.RemoveAll(tmpDir)
	if err := verifySignature(evaluated, tmpDir, localSources, publicKey); err != nil {
		return exitErr(ExitSignature, err)
	}
	fmt.Fprintf(out, "=======> Signature check succeeded.\n")
	return nil
}

// UpdateSignature generates/updates <portDir>/.signature using signify over SHA-256
// hashes of Pkgfile, .footprint, and sources (matching CRUX `pkgmk -us` / `-rs`).
func UpdateSignature(cfg config.Config, port ports.Port, secretKey string, keepExistingChecksums bool, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return exitErr(ExitPkgfile, err)
	}
	signifyPath, err := exec.LookPath("signify")
	if err != nil {
		return exitErr(ExitSignature, fmt.Errorf("signify not found in PATH"))
	}
	if strings.TrimSpace(secretKey) == "" {
		// Default CRUX location: /etc/ports/<collection>.sec
		if evaluated.Collection != "" {
			secretKey = filepath.Join("/etc/ports", evaluated.Collection+".sec")
		}
	}
	if strings.TrimSpace(secretKey) == "" {
		return exitErr(ExitSignature, fmt.Errorf("secret key required (pass -sk <file>)"))
	}
	sigPath := filepath.Join(evaluated.Dir, ".signature")
	var payload []byte
	if keepExistingChecksums {
		raw, err := os.ReadFile(sigPath)
		if err != nil {
			return exitErr(ExitSignature, fmt.Errorf("read existing .signature for refresh: %w", err))
		}
		// Strip first 2 signify header lines to re-sign existing checksum body.
		lines := strings.Split(string(raw), "\n")
		if len(lines) > 2 {
			payload = []byte(strings.Join(lines[2:], "\n"))
		}
	} else {
		localSources, err := resolveAndDownloadSources(cfg, evaluated, out)
		if err != nil {
			return exitErr(ExitDownload, err)
		}
		files := []string{filepath.Join(evaluated.Dir, "Pkgfile")}
		if _, statErr := os.Stat(filepath.Join(evaluated.Dir, ".footprint")); statErr == nil {
			files = append(files, filepath.Join(evaluated.Dir, ".footprint"))
		}
		files = append(files, localSources...)
		var buf bytes.Buffer
		for _, f := range files {
			sum, serr := sha256File(f)
			if serr != nil {
				return exitErr(ExitSignature, serr)
			}
			fmt.Fprintf(&buf, "SHA256 (%s) = %s\n", filepath.Base(f), sum)
		}
		payload = buf.Bytes()
	}

	tmpIn, err := os.CreateTemp("", "kpm-sign-in-*")
	if err != nil {
		return exitErr(ExitSignature, err)
	}
	tmpInName := tmpIn.Name()
	defer os.Remove(tmpInName)
	if _, err := tmpIn.Write(payload); err != nil {
		_ = tmpIn.Close()
		return exitErr(ExitSignature, err)
	}
	_ = tmpIn.Close()

	cmd := exec.Command(signifyPath, "-S", "-e", "-s", secretKey, "-m", tmpInName, "-x", sigPath)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return exitErr(ExitSignature, fmt.Errorf("signify signing failed: %s (%w)", strings.TrimSpace(string(combined)), err))
	}
	fmt.Fprintf(out, "=======> Signature updated.\n")
	return nil
}

// CleanPort removes the built package archive and downloaded remote source files
// for port (matching CRUX `pkgmk -c`).
func CleanPort(cfg config.Config, port ports.Port, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	evaluated, err := ports.ParsePkgfile(port.Dir, cfg.Paths.PortsDir)
	if err != nil {
		return exitErr(ExitPkgfile, err)
	}
	target := TargetArchivePath(cfg, evaluated)
	if _, err := os.Stat(target); err == nil {
		fmt.Fprintf(out, "=======> Removing '%s'.\n", target)
		if rerr := os.Remove(target); rerr != nil {
			return exitErr(ExitGeneral, rerr)
		}
	}
	for i, src := range evaluated.Source {
		if !isRemoteSource(src) {
			continue
		}
		localPath := resolveLocalSourcePath(src, renameAt(evaluated.Renames, i), evaluated.Dir, cfg.Paths.SourceCache)
		if _, err := os.Stat(localPath); err == nil {
			fmt.Fprintf(out, "=======> Removing '%s'.\n", localPath)
			if rerr := os.Remove(localPath); rerr != nil {
				return exitErr(ExitGeneral, rerr)
			}
		}
	}
	return nil
}

// DiscoverRecursivePorts finds all directories containing Pkgfile under baseDir
// in deterministic sorted order (for `pkgmk -r`).
func DiscoverRecursivePorts(baseDir string) ([]string, error) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, err
	}
	var dirs []string
	err = filepath.Walk(absBase, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() && strings.HasPrefix(info.Name(), ".") && path != absBase {
			return filepath.SkipDir
		}
		if !info.IsDir() && info.Name() == "Pkgfile" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
