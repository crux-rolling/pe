// Package archive handles CRUX Linux package archives (<name>#<version>-<release>.pkg.tar.<ext>),
// including inspection, footprint generation, extraction with pkgadd.conf keep/reject policy, and creation.
package archive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ezke5/pe/internal/pkgdb"
)

const (
	PkgExt       = ".pkg.tar."
	VersionDelim = "#"
)

// SupportedExtensions lists all compression suffixes supported by CRUX pkgutils.
var SupportedExtensions = []string{"gz", "bz2", "xz", "lz", "zst"}

// IsSupportedExt reports whether ext is a valid CRUX package compression suffix.
func IsSupportedExt(ext string) bool {
	for _, s := range SupportedExtensions {
		if ext == s {
			return true
		}
	}
	return false
}

// ParsePkgFilename parses a CRUX package archive filename of the form:
// <name>#<version>-<release>.pkg.tar.<ext>
// Returns name, fullVersion (e.g. "1.2.3-1"), ext (e.g. "zst"), and ok.
func ParsePkgFilename(filename string) (name, fullVersion, ext string, ok bool) {
	base := filepath.Base(filename)
	extIdx := strings.LastIndex(base, PkgExt)
	if extIdx <= 0 {
		return "", "", "", false
	}
	ext = base[extIdx+len(PkgExt):]
	if !IsSupportedExt(ext) {
		return "", "", "", false
	}
	stem := base[:extIdx]
	hashIdx := strings.Index(stem, VersionDelim)
	if hashIdx <= 0 || hashIdx >= len(stem)-1 {
		return "", "", "", false
	}
	name = stem[:hashIdx]
	fullVersion = stem[hashIdx+1:]
	if name == "" || fullVersion == "" {
		return "", "", "", false
	}
	return name, fullVersion, ext, true
}

// SplitVersionRelease splits a CRUX version string "<version>-<release>" on the last '-'.
func SplitVersionRelease(fullVersion string) (version, release string, ok bool) {
	idx := strings.LastIndex(fullVersion, "-")
	if idx <= 0 || idx >= len(fullVersion)-1 {
		return fullVersion, "", false
	}
	return fullVersion[:idx], fullVersion[idx+1:], true
}

// FormatPkgFilename builds a CRUX package archive filename.
func FormatPkgFilename(name, version, release, compression string) string {
	if release == "" {
		return fmt.Sprintf("%s#%s.pkg.tar.%s", name, version, compression)
	}
	return fmt.Sprintf("%s#%s-%s.pkg.tar.%s", name, version, release, compression)
}

// Open inspects a CRUX package archive and returns its package name and PkgInfo
// (version and file list), mirroring pkgutil::pkg_open.
func Open(filename string) (string, pkgdb.PkgInfo, error) {
	name, version, _, ok := ParsePkgFilename(filename)
	if !ok {
		return "", pkgdb.PkgInfo{}, fmt.Errorf("could not determine name and/or version of %s: Invalid package name", filepath.Base(filename))
	}

	ar, err := openTarStream(filename)
	if err != nil {
		return "", pkgdb.PkgInfo{}, fmt.Errorf("could not open %s: %w", filename, err)
	}
	defer ar.Close()

	files := make(map[string]struct{})
	count := 0
	for {
		hdr, err := ar.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", pkgdb.PkgInfo{}, fmt.Errorf("could not read %s: %w", filename, err)
		}
		p, err := sanitizeArchivePath(hdr.Name, hdr.Typeflag == tar.TypeDir)
		if err != nil {
			return "", pkgdb.PkgInfo{}, fmt.Errorf("invalid path in %s: %w", filename, err)
		}
		if p == "" {
			continue
		}
		files[p] = struct{}{}
		count++
	}
	if err := ar.Close(); err != nil {
		return "", pkgdb.PkgInfo{}, fmt.Errorf("could not read %s: %w", filename, err)
	}
	if count == 0 {
		return "", pkgdb.PkgInfo{}, fmt.Errorf("empty package")
	}

	return name, pkgdb.PkgInfo{
		Version: version,
		Files:   files,
	}, nil
}

// Extract installs archive contents into root following CRUX pkgutil::pkg_install policy:
// - Files in nonInstallList are ignored and logged.
// - Existing files in keepList are extracted to <root>/var/lib/pkg/rejected/ and pruned if
//   identical/empty with matching permissions; otherwise a rejection notice is printed.
// - On non-upgrade installs, any extraction error aborts immediately; on upgrades it logs and continues.
func Extract(filename, root string, keepList, nonInstallList map[string]struct{}, upgrade bool, out, errOut io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve root %q: %w", root, err)
	}
	rejectDir := filepath.Join(absRoot, pkgdb.PkgRejected)

	ar, err := openTarStream(filename)
	if err != nil {
		return fmt.Errorf("could not open %s: %w", filename, err)
	}
	defer ar.Close()

	isRootUser := os.Geteuid() == 0
	count := 0

	for {
		hdr, err := ar.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("could not read %s: %w", filename, err)
		}
		archivePath, err := sanitizeArchivePath(hdr.Name, hdr.Typeflag == tar.TypeDir)
		if err != nil {
			return fmt.Errorf("invalid archive path %q: %w", hdr.Name, err)
		}
		if archivePath == "" {
			continue
		}
		count++

		if _, skip := nonInstallList[archivePath]; skip {
			fmt.Fprintf(out, "pe: ignoring %s\n", archivePath)
			continue
		}

		originalPath := filepath.Join(absRoot, archivePath)
		realPath := originalPath

		if fileExists(realPath) {
			if _, keep := keepList[archivePath]; keep {
				realPath = filepath.Join(rejectDir, archivePath)
			}
		}

		if err := extractEntry(ar.tr, hdr, absRoot, realPath, isRootUser); err != nil {
			fmt.Fprintf(errOut, "pe: could not install %s: %v\n", archivePath, err)
			if !upgrade {
				return fmt.Errorf("extract error: %s: %w", archivePath, err)
			}
			continue
		}

		if realPath != originalPath {
			removeRejected := false
			if hdr.Typeflag == tar.TypeDir {
				removeRejected = permissionsEqual(realPath, originalPath)
			} else {
				removeRejected = permissionsEqual(realPath, originalPath) &&
					(fileEmpty(realPath) || fileEqual(realPath, originalPath))
			}
			if removeRejected {
				removeEmptyParents(rejectDir, realPath)
			} else {
				fmt.Fprintf(out, "pe: rejecting %s, keeping existing version\n", archivePath)
			}
		}
	}

	if err := ar.Close(); err != nil {
		return fmt.Errorf("could not read %s: %w", filename, err)
	}
	if count == 0 {
		return fmt.Errorf("empty package")
	}
	return nil
}

// Footprint writes a CRUX pkginfo --footprint formatted listing for filename.
func Footprint(filename string, w io.Writer) error {
	// Pass 1: record modes for non-hardlink entries so hardlinks use target mode.
	hardlinkModes := make(map[string]os.FileMode)
	ar1, err := openTarStream(filename)
	if err != nil {
		return fmt.Errorf("could not open %s: %w", filename, err)
	}
	for {
		hdr, err := ar1.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = ar1.Close()
			return fmt.Errorf("could not read %s: %w", filename, err)
		}
		if hdr.Typeflag != tar.TypeLink {
			p, err := sanitizeArchivePath(hdr.Name, hdr.Typeflag == tar.TypeDir)
			if err == nil && p != "" {
				hardlinkModes[p] = tarHeaderMode(hdr)
			}
		}
	}
	if err := ar1.Close(); err != nil {
		return fmt.Errorf("could not read %s: %w", filename, err)
	}

	// Pass 2: emit CRUX footprint lines.
	ar2, err := openTarStream(filename)
	if err != nil {
		return fmt.Errorf("could not open %s: %w", filename, err)
	}
	defer ar2.Close()

	bw := bufio.NewWriter(w)
	count := 0
	for {
		hdr, err := ar2.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("could not read %s: %w", filename, err)
		}
		p, err := sanitizeArchivePath(hdr.Name, hdr.Typeflag == tar.TypeDir)
		if err != nil {
			return err
		}
		if p == "" {
			continue
		}
		count++

		var modeStr string
		if hdr.Typeflag == tar.TypeSymlink {
			modeStr = "lrwxrwxrwx"
		} else if hdr.Typeflag == tar.TypeLink {
			target, _ := sanitizeArchivePath(hdr.Linkname, false)
			if tm, ok := hardlinkModes[target]; ok {
				modeStr = formatMode(tm, tar.TypeReg)
			} else {
				modeStr = formatMode(tarHeaderMode(hdr), tar.TypeReg)
			}
		} else {
			modeStr = formatMode(tarHeaderMode(hdr), hdr.Typeflag)
		}

		uname := lookupUser(hdr.Uid, hdr.Uname)
		gname := lookupGroup(hdr.Gid, hdr.Gname)

		var extra string
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			extra = " -> " + hdr.Linkname
		case tar.TypeChar, tar.TypeBlock:
			extra = fmt.Sprintf(" (%d, %d)", hdr.Devmajor, hdr.Devminor)
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size == 0 {
				extra = " (EMPTY)"
			}
		}
		fmt.Fprintf(bw, "%s\t%s/%s\t%s%s\n", modeStr, uname, gname, p, extra)
	}
	if err := ar2.Close(); err != nil {
		return fmt.Errorf("could not read %s: %w", filename, err)
	}
	if count == 0 {
		return fmt.Errorf("empty package")
	}
	return bw.Flush()
}

// Create builds a CRUX package archive from srcDir using bsdtar --format=gnutar,
// matching pkgmk's packaging command.
func Create(srcDir, targetPath, compression string) error {
	if !IsSupportedExt(compression) {
		return fmt.Errorf("unsupported compression mode %q", compression)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("read package staging dir %q: %w", srcDir, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("package staging directory %q is empty", srcDir)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create package output dir: %w", err)
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var compFlag string
	switch compression {
	case "gz":
		compFlag = "-z"
	case "bz2":
		compFlag = "-j"
	case "xz":
		compFlag = "-J"
	case "lz":
		compFlag = "--lzip"
	case "zst":
		compFlag = "--zstd"
	}

	tool := "bsdtar"
	args := []string{"--format=gnutar", "-c", compFlag, "-f", absTarget}
	if _, err := exec.LookPath("bsdtar"); err != nil {
		tool = "tar"
		args = []string{"-c", compFlag, "-f", absTarget}
	}
	args = append(args, names...)

	cmd := exec.Command(tool, args...)
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s archive creation failed: %w: %s", tool, err, strings.TrimSpace(string(out)))
	}
	return nil
}

type tarStream struct {
	tr     *tar.Reader
	closers []io.Closer
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	closed bool
}

func (s *tarStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var firstErr error
	for _, c := range s.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.cmd != nil {
		if err := s.cmd.Wait(); err != nil && firstErr == nil {
			msg := strings.TrimSpace(s.stderr.String())
			if msg != "" {
				firstErr = fmt.Errorf("%w: %s", err, msg)
			} else {
				firstErr = err
			}
		}
	}
	return firstErr
}

func openTarStream(filename string) (*tarStream, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	ext := detectCompression(filename)
	switch ext {
	case "gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		return &tarStream{
			tr:      tar.NewReader(gr),
			closers: []io.Closer{gr, f},
		}, nil
	case "bz2":
		br := bzip2.NewReader(f)
		return &tarStream{
			tr:      tar.NewReader(br),
			closers: []io.Closer{f},
		}, nil
	case "xz":
		_ = f.Close()
		return openExternalFilter("xz", []string{"-dc", filename})
	case "zst":
		_ = f.Close()
		return openExternalFilter("zstd", []string{"-dc", "-q", filename})
	case "lz":
		_ = f.Close()
		return openExternalFilter("lzip", []string{"-dc", filename})
	default:
		return &tarStream{
			tr:      tar.NewReader(f),
			closers: []io.Closer{f},
		}, nil
	}
}

func openExternalFilter(bin string, args []string) (*tarStream, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("required decompression tool %q not found in PATH", bin)
	}
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &tarStream{
		tr:      tar.NewReader(stdout),
		closers: []io.Closer{stdout},
		cmd:     cmd,
		stderr:  &stderr,
	}, nil
}

func detectCompression(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".gz"), strings.HasSuffix(lower, ".tgz"):
		return "gz"
	case strings.HasSuffix(lower, ".bz2"), strings.HasSuffix(lower, ".tbz2"):
		return "bz2"
	case strings.HasSuffix(lower, ".xz"), strings.HasSuffix(lower, ".txz"):
		return "xz"
	case strings.HasSuffix(lower, ".zst"), strings.HasSuffix(lower, ".tzst"):
		return "zst"
	case strings.HasSuffix(lower, ".lz"):
		return "lz"
	default:
		return ""
	}
}

func sanitizeArchivePath(raw string, isDir bool) (string, error) {
	p := strings.TrimPrefix(raw, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == "" {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("unsafe path %q", raw)
	}
	if isDir && !strings.HasSuffix(clean, "/") {
		clean += "/"
	}
	return clean, nil
}

func extractEntry(tr *tar.Reader, hdr *tar.Header, absRoot, realPath string, isRootUser bool) error {
	parent := filepath.Dir(realPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}

	mode := os.FileMode(hdr.Mode & 0o7777)

	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(realPath, mode.Perm()); err != nil {
			return err
		}
		if err := chmodFull(realPath, uint32(hdr.Mode&0o7777)); err != nil {
			return err
		}
		if isRootUser {
			_ = os.Lchown(realPath, hdr.Uid, hdr.Gid)
		}
		applyTimes(realPath, hdr.AccessTime, hdr.ModTime)
		return nil

	case tar.TypeReg, tar.TypeRegA:
		_ = os.Remove(realPath)
		f, err := os.OpenFile(realPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if isRootUser {
			_ = os.Lchown(realPath, hdr.Uid, hdr.Gid)
		}
		// Apply mode after chown because chown clears setuid/setgid bits on Linux.
		if err := chmodFull(realPath, uint32(hdr.Mode&0o7777)); err != nil {
			return err
		}
		applyTimes(realPath, hdr.AccessTime, hdr.ModTime)
		return nil

	case tar.TypeSymlink:
		_ = os.Remove(realPath)
		if err := os.Symlink(hdr.Linkname, realPath); err != nil {
			return err
		}
		if isRootUser {
			_ = os.Lchown(realPath, hdr.Uid, hdr.Gid)
		}
		return nil

	case tar.TypeLink:
		targetRel, err := sanitizeArchivePath(hdr.Linkname, false)
		if err != nil {
			return err
		}
		target := filepath.Join(absRoot, targetRel)
		_ = os.Remove(realPath)
		return os.Link(target, realPath)

	case tar.TypeFifo:
		_ = os.Remove(realPath)
		if err := syscall.Mkfifo(realPath, uint32(hdr.Mode&0o7777)); err != nil {
			return err
		}
		if isRootUser {
			_ = os.Lchown(realPath, hdr.Uid, hdr.Gid)
		}
		return nil

	case tar.TypeChar, tar.TypeBlock:
		_ = os.Remove(realPath)
		devType := uint32(syscall.S_IFCHR)
		if hdr.Typeflag == tar.TypeBlock {
			devType = syscall.S_IFBLK
		}
		dev := int((hdr.Devmajor << 8) | (hdr.Devminor & 0xff))
		if err := syscall.Mknod(realPath, devType|uint32(hdr.Mode&0o7777), dev); err != nil {
			return err
		}
		if isRootUser {
			_ = os.Lchown(realPath, hdr.Uid, hdr.Gid)
		}
		return nil

	case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
		return nil

	default:
		return fmt.Errorf("unsupported tar entry type %q", hdr.Typeflag)
	}
}

func chmodFull(path string, mode uint32) error {
	return syscall.Chmod(path, mode)
}

func applyTimes(path string, atime, mtime time.Time) {
	if mtime.IsZero() {
		return
	}
	if atime.IsZero() {
		atime = mtime
	}
	_ = os.Chtimes(path, atime, mtime)
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func fileEmpty(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() && info.Size() == 0
}

func permissionsEqual(path1, path2 string) bool {
	var st1, st2 syscall.Stat_t
	if err := syscall.Lstat(path1, &st1); err != nil {
		return false
	}
	if err := syscall.Lstat(path2, &st2); err != nil {
		return false
	}
	return st1.Mode == st2.Mode && st1.Uid == st2.Uid && st1.Gid == st2.Gid
}

func fileEqual(path1, path2 string) bool {
	var st1, st2 syscall.Stat_t
	if err := syscall.Lstat(path1, &st1); err != nil {
		return false
	}
	if err := syscall.Lstat(path2, &st2); err != nil {
		return false
	}
	type1 := st1.Mode & syscall.S_IFMT
	type2 := st2.Mode & syscall.S_IFMT
	if type1 != type2 {
		return false
	}
	switch type1 {
	case syscall.S_IFREG:
		if st1.Size != st2.Size {
			return false
		}
		f1, err := os.Open(path1)
		if err != nil {
			return false
		}
		defer f1.Close()
		f2, err := os.Open(path2)
		if err != nil {
			return false
		}
		defer f2.Close()
		buf1 := make([]byte, 4096)
		buf2 := make([]byte, 4096)
		for {
			n1, err1 := io.ReadFull(f1, buf1)
			n2, err2 := io.ReadFull(f2, buf2)
			if n1 != n2 || !bytes.Equal(buf1[:n1], buf2[:n2]) {
				return false
			}
			if errors.Is(err1, io.EOF) || errors.Is(err1, io.ErrUnexpectedEOF) {
				return errors.Is(err2, io.EOF) || errors.Is(err2, io.ErrUnexpectedEOF)
			}
			if err1 != nil || err2 != nil {
				return false
			}
		}
	case syscall.S_IFLNK:
		l1, err1 := os.Readlink(path1)
		l2, err2 := os.Readlink(path2)
		return err1 == nil && err2 == nil && l1 == l2
	case syscall.S_IFCHR, syscall.S_IFBLK:
		return st1.Rdev == st2.Rdev
	default:
		return false
	}
}

func removeEmptyParents(baseDir, path string) {
	cleanBase := filepath.Clean(baseDir)
	cur := filepath.Clean(path)
	for cur != cleanBase && strings.HasPrefix(cur, cleanBase+string(os.PathSeparator)) {
		if err := os.Remove(cur); err != nil {
			break
		}
		cur = filepath.Dir(cur)
	}
}

func tarHeaderMode(hdr *tar.Header) os.FileMode {
	return os.FileMode(hdr.Mode)
}

func formatMode(mode os.FileMode, typeflag byte) string {
	var b [10]byte
	switch typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeLink:
		b[0] = '-'
	case tar.TypeDir:
		b[0] = 'd'
	case tar.TypeSymlink:
		b[0] = 'l'
	case tar.TypeChar:
		b[0] = 'c'
	case tar.TypeBlock:
		b[0] = 'b'
	case tar.TypeFifo:
		b[0] = 'p'
	default:
		b[0] = '?'
	}

	raw := uint32(mode)
	b[1] = permBit(raw, 0o400, 'r')
	b[2] = permBit(raw, 0o200, 'w')
	b[3] = execBit(raw, 0o100, 0o4000, 's', 'S')

	b[4] = permBit(raw, 0o040, 'r')
	b[5] = permBit(raw, 0o020, 'w')
	b[6] = execBit(raw, 0o010, 0o2000, 's', 'S')

	b[7] = permBit(raw, 0o004, 'r')
	b[8] = permBit(raw, 0o002, 'w')
	b[9] = execBit(raw, 0o001, 0o1000, 't', 'T')
	return string(b[:])
}

func permBit(mode, bit uint32, ch byte) byte {
	if mode&bit != 0 {
		return ch
	}
	return '-'
}

func execBit(mode, xBit, sBit uint32, both, onlyS byte) byte {
	hasX := mode&xBit != 0
	hasS := mode&sBit != 0
	switch {
	case hasX && hasS:
		return both
	case hasS:
		return onlyS
	case hasX:
		return 'x'
	default:
		return '-'
	}
}

func lookupUser(uid int, fallback string) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return u.Username
	}
	if fallback != "" {
		return fallback
	}
	return strconv.Itoa(uid)
}

func lookupGroup(gid int, fallback string) string {
	if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil && g.Name != "" {
		return g.Name
	}
	if fallback != "" {
		return fallback
	}
	return strconv.Itoa(gid)
}
