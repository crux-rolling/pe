// Package rejected implements CRUX rejmerge-compatible inspection, diffing,
// merging, and cleanup of files under <root>/var/lib/pkg/rejected/, shared by
// both `kpm rejected` and the `rejmerge` multi-call symlink.
package rejected

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/kryx-linux/kpm/internal/pkgdb"
)

// Options controls rejected file operations.
type Options struct {
	Root        string
	ProgName    string // "rejmerge" or "kpm"
	In          io.Reader
	Out         io.Writer
	ErrOut      io.Writer
	Interactive bool
}

// Entry represents one pending rejected file relative to <root>.
type Entry struct {
	RelPath       string // e.g. "etc/rc.conf"
	InstalledPath string // <root>/etc/rc.conf
	RejectedPath  string // <root>/var/lib/pkg/rejected/etc/rc.conf
}

// RejectedDir returns <root>/var/lib/pkg/rejected.
func RejectedDir(root string) string {
	if strings.TrimSpace(root) == "" {
		root = "/"
	}
	return filepath.Join(root, pkgdb.PkgRejected)
}

// List scans <root>/var/lib/pkg/rejected/ and returns all pending rejected entries
// in sorted order.
func List(root string) ([]Entry, error) {
	absRoot, err := filepath.Abs(normalizeRoot(root))
	if err != nil {
		return nil, err
	}
	rejDir := filepath.Join(absRoot, pkgdb.PkgRejected)
	info, err := os.Stat(rejDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	var entries []Entry
	err = filepath.WalkDir(rejDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rejDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		entries = append(entries, Entry{
			RelPath:       rel,
			InstalledPath: filepath.Join(absRoot, filepath.FromSlash(rel)),
			RejectedPath:  path,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelPath < entries[j].RelPath })
	return entries, nil
}

// Diff returns a unified diff between the installed file and the rejected file.
func Diff(e Entry) (string, error) {
	if _, err := exec.LookPath("diff"); err == nil {
		cmd := exec.Command("diff", "-u", e.InstalledPath, e.RejectedPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			var exitErr *exec.ExitError
			// diff exits 1 when files differ, which is normal.
			if !errors.As(err, &exitErr) || exitErr.ExitCode() > 1 {
				return "", fmt.Errorf("diff %s: %w: %s", e.RelPath, err, strings.TrimSpace(string(out)))
			}
		}
		return string(out), nil
	}
	// Pure-Go fallback diff when external diff binary is absent.
	a, err := os.ReadFile(e.InstalledPath)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(e.RejectedPath)
	if err != nil {
		return "", err
	}
	return simpleUnifiedDiff(e.InstalledPath, e.RejectedPath, string(a), string(b)), nil
}

// Keep retains the installed file and removes the rejected copy (plus empty parent dirs).
func Keep(root string, e Entry) error {
	if err := os.Remove(e.RejectedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove rejected %s: %w", e.RelPath, err)
	}
	CleanEmptyDirs(root)
	return nil
}

// Upgrade replaces the installed file with the rejected file (and cleans empty parent dirs).
func Upgrade(root string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(e.InstalledPath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(e.RejectedPath, e.InstalledPath); err != nil {
		// Cross-device fallback: copy + remove.
		if cerr := copyEntry(e.RejectedPath, e.InstalledPath); cerr != nil {
			return fmt.Errorf("upgrade %s: %w", e.RelPath, cerr)
		}
		_ = os.Remove(e.RejectedPath)
	}
	CleanEmptyDirs(root)
	return nil
}

// MergeContent produces a CRUX rejmerge-style conflict-marked merge of installedPath
// and rejectedPath using `diff` (matching upstream rejmerge.in's --old-group-format
// / --new-group-format / --changed-group-format) with a pure-Go fallback.
func MergeContent(installedPath, rejectedPath string) ([]byte, bool, error) {
	if _, err := exec.LookPath("diff"); err == nil {
		cmd := exec.Command("diff",
			"--old-group-format=<<<<< MERGE CONFLICT (installed) "+installedPath+"\n%<>>>>>\n",
			"--new-group-format=<<<<< MERGE CONFLICT (rejected) "+rejectedPath+"\n%>>>>>>\n",
			"--changed-group-format=<<<<< MERGE CONFLICT (installed) "+installedPath+"\n%<=====\n%>>>>>> "+rejectedPath+"\n",
			installedPath, rejectedPath,
		)
		out, err := cmd.Output()
		if err == nil {
			return out, false, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return out, true, nil
		}
	}
	// Pure-Go fallback merge.
	a, err := os.ReadFile(installedPath)
	if err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(rejectedPath)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(a, b) {
		return a, false, nil
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "<<<<< MERGE CONFLICT (installed) %s\n", installedPath)
	buf.Write(a)
	if len(a) > 0 && a[len(a)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString("=====\n")
	buf.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		buf.WriteByte('\n')
	}
	fmt.Fprintf(&buf, ">>>>> %s\n", rejectedPath)
	return buf.Bytes(), true, nil
}

// MergeFile non-interactively merges e.InstalledPath and e.RejectedPath, writing the
// conflict-marked result into e.InstalledPath and removing the rejected copy.
func MergeFile(root string, e Entry) (hadConflicts bool, err error) {
	merged, conflicts, err := MergeContent(e.InstalledPath, e.RejectedPath)
	if err != nil {
		return false, err
	}
	fi, statErr := os.Lstat(e.InstalledPath)
	mode := os.FileMode(0o644)
	if statErr == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(e.InstalledPath, merged, mode); err != nil {
		return conflicts, fmt.Errorf("write merged %s: %w", e.RelPath, err)
	}
	if err := os.Remove(e.RejectedPath); err != nil && !os.IsNotExist(err) {
		return conflicts, err
	}
	CleanEmptyDirs(root)
	return conflicts, nil
}

// CleanEmptyDirs removes empty directories inside <root>/var/lib/pkg/rejected/
// (leaving the rejected root directory itself intact).
func CleanEmptyDirs(root string) {
	rejDir := filepath.Clean(RejectedDir(root))
	var dirs []string
	_ = filepath.WalkDir(rejDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && filepath.Clean(path) != rejDir {
			dirs = append(dirs, path)
		}
		return nil
	})
	// Remove deepest directories first.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}

// RunInteractive executes the full upstream CRUX rejmerge interactive session:
// auto-pruning missing/identical files, offering permissions menu, diff/file/merge menus,
// and cleaning up empty directories.
func RunInteractive(opts Options) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.ErrOut == nil {
		opts.ErrOut = os.Stderr
	}
	prog := opts.ProgName
	if prog == "" {
		prog = "rejmerge"
	}

	entries, err := List(opts.Root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintf(opts.Out, "%s: nothing to merge\n", prog)
		return nil
	}

	reader := bufio.NewReader(opts.In)
	prompt := func(msg string) (string, error) {
		fmt.Fprint(opts.Out, msg)
		line, rerr := reader.ReadString('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return "", rerr
		}
		if rerr != nil && line == "" {
			return "", io.EOF
		}
		return strings.ToUpper(strings.TrimSpace(line)), nil
	}

	var skipped []string
	for _, e := range entries {
		instInfo, instErr := os.Lstat(e.InstalledPath)
		rejInfo, rejErr := os.Lstat(e.RejectedPath)
		if rejErr != nil {
			continue
		}
		// Remove rejected file if installed counterpart no longer exists.
		if os.IsNotExist(instErr) {
			_ = os.Remove(e.RejectedPath)
			continue
		}
		if instErr != nil {
			fmt.Fprintf(opts.ErrOut, "%s: cannot stat %s: %v\n", prog, e.InstalledPath, instErr)
			skipped = append(skipped, "/"+e.RelPath)
			continue
		}

		// Check permissions difference first (matches upstream permissions_menu).
		if !permsMatch(e.InstalledPath, e.RejectedPath) {
			if err := runPermissionsMenu(opts.Out, prompt, e); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
		}

		// Auto-prune if both are regular files with identical content (or both symlinks with same target).
		if filesIdentical(e.InstalledPath, e.RejectedPath, instInfo, rejInfo) {
			_ = os.Remove(e.RejectedPath)
			continue
		}

		// Regular files get diff + merge menu; special files/symlinks get file menu.
		if instInfo.Mode().IsRegular() && rejInfo.Mode().IsRegular() {
			done := false
			showDiff := true
			for !done {
				if showDiff {
					if d, derr := Diff(e); derr == nil && d != "" {
						fmt.Fprint(opts.Out, d)
					}
					showDiff = false
				}
				ans, perr := prompt(fmt.Sprintf("%s [K]eep [U]pgrade [M]erge [D]iff [S]kip? ", e.InstalledPath))
				if perr != nil {
					if errors.Is(perr, io.EOF) {
						skipped = append(skipped, "/"+e.RelPath)
						done = true
						break
					}
					return perr
				}
				switch ans {
				case "K":
					if err := Keep(opts.Root, e); err != nil {
						return err
					}
					done = true
				case "U":
					if err := Upgrade(opts.Root, e); err != nil {
						return err
					}
					done = true
				case "M":
					handled, merr := runMergeMenu(opts.Root, opts.Out, opts.ErrOut, prompt, e)
					if merr != nil {
						if errors.Is(merr, io.EOF) {
							skipped = append(skipped, "/"+e.RelPath)
							done = true
							break
						}
						return merr
					}
					if handled {
						done = true
					}
				case "D":
					showDiff = true
				case "S":
					skipped = append(skipped, "/"+e.RelPath)
					done = true
				}
			}
		} else {
			done := false
			for !done {
				fmt.Fprintf(opts.Out, "Installed: %s (%s)\nRejected:  %s (%s)\n",
					e.InstalledPath, describeMode(instInfo, e.InstalledPath),
					e.RejectedPath, describeMode(rejInfo, e.RejectedPath))
				ans, perr := prompt(fmt.Sprintf("%s [K]eep [U]pgrade [S]kip? ", e.InstalledPath))
				if perr != nil {
					if errors.Is(perr, io.EOF) {
						skipped = append(skipped, "/"+e.RelPath)
						done = true
						break
					}
					return perr
				}
				switch ans {
				case "K":
					if err := Keep(opts.Root, e); err != nil {
						return err
					}
					done = true
				case "U":
					if err := Upgrade(opts.Root, e); err != nil {
						return err
					}
					done = true
				case "S":
					skipped = append(skipped, "/"+e.RelPath)
					done = true
				}
			}
		}
	}

	CleanEmptyDirs(opts.Root)

	if len(skipped) > 0 {
		fmt.Fprintf(opts.Out, "%s: skipped files:\n", prog)
		for _, s := range skipped {
			fmt.Fprintf(opts.Out, "  %s\n", s)
		}
	}
	return nil
}

func runPermissionsMenu(out io.Writer, prompt func(string) (string, error), e Entry) error {
	for {
		fmt.Fprintf(out, "Permissions differ for %s:\n", e.InstalledPath)
		fmt.Fprintf(out, "  installed: %s\n", formatStatPerms(e.InstalledPath))
		fmt.Fprintf(out, "  rejected:  %s\n", formatStatPerms(e.RejectedPath))
		ans, err := prompt("Permissions [K]eep [U]pgrade [S]kip? ")
		if err != nil {
			return err
		}
		switch ans {
		case "K":
			return syncPerms(e.InstalledPath, e.RejectedPath)
		case "U":
			return syncPerms(e.RejectedPath, e.InstalledPath)
		case "S":
			return nil
		}
	}
}

func runMergeMenu(root string, out, errOut io.Writer, prompt func(string) (string, error), e Entry) (bool, error) {
	merged, _, err := MergeContent(e.InstalledPath, e.RejectedPath)
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp("", "rejmerge-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(merged); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	runEditor := func() {
		editor := strings.TrimSpace(os.Getenv("EDITOR"))
		if editor == "" {
			return
		}
		parts := strings.Fields(editor)
		if len(parts) == 0 {
			return
		}
		if _, err := exec.LookPath(parts[0]); err != nil {
			return
		}
		cmd := exec.Command(parts[0], append(parts[1:], tmpPath)...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = out
		cmd.Stderr = errOut
		_ = cmd.Run()
	}

	for {
		ans, perr := prompt(fmt.Sprintf("Merged %s [I]nstall [E]dit [V]iew [S]kip? ", e.InstalledPath))
		if perr != nil {
			return false, perr
		}
		switch ans {
		case "I":
			data, rerr := os.ReadFile(tmpPath)
			if rerr != nil {
				return false, rerr
			}
			fi, _ := os.Lstat(e.InstalledPath)
			mode := os.FileMode(0o644)
			if fi != nil {
				mode = fi.Mode().Perm()
			}
			if err := os.WriteFile(e.InstalledPath, data, mode); err != nil {
				return false, err
			}
			_ = os.Remove(e.RejectedPath)
			CleanEmptyDirs(root)
			return true, nil
		case "E":
			runEditor()
		case "V":
			if data, rerr := os.ReadFile(tmpPath); rerr == nil {
				fmt.Fprint(out, string(data))
			}
		case "S":
			return false, nil
		}
	}
}

// ResolveTarget filters entries by a target string ("all", "/etc/foo", "etc/foo", or suffix).
func ResolveTarget(entries []Entry, target string) ([]Entry, error) {
	t := strings.TrimSpace(target)
	if t == "" || strings.EqualFold(t, "all") {
		return entries, nil
	}
	norm := strings.TrimPrefix(filepath.ToSlash(t), "/")
	var matched []Entry
	for _, e := range entries {
		if e.RelPath == norm || strings.HasSuffix("/"+e.RelPath, "/"+norm) {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("rejected file %q not found", target)
	}
	return matched, nil
}

func permsMatch(p1, p2 string) bool {
	var s1, s2 syscall.Stat_t
	if syscall.Lstat(p1, &s1) != nil || syscall.Lstat(p2, &s2) != nil {
		return true
	}
	return s1.Mode == s2.Mode && s1.Uid == s2.Uid && s1.Gid == s2.Gid
}

func formatStatPerms(path string) string {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return "unknown"
	}
	return fmt.Sprintf("mode=%04o uid=%d gid=%d", st.Mode&0o7777, st.Uid, st.Gid)
}

func syncPerms(src, dst string) error {
	var st syscall.Stat_t
	if err := syscall.Lstat(src, &st); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		_ = os.Lchown(dst, int(st.Uid), int(st.Gid))
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
		return syscall.Chmod(dst, st.Mode&0o7777)
	}
	return nil
}

func filesIdentical(p1, p2 string, i1, i2 os.FileInfo) bool {
	if i1.Mode()&os.ModeType != i2.Mode()&os.ModeType {
		return false
	}
	if i1.Mode()&os.ModeSymlink != 0 {
		l1, e1 := os.Readlink(p1)
		l2, e2 := os.Readlink(p2)
		return e1 == nil && e2 == nil && l1 == l2
	}
	if !i1.Mode().IsRegular() {
		return false
	}
	if i1.Size() != i2.Size() {
		return false
	}
	b1, e1 := os.ReadFile(p1)
	b2, e2 := os.ReadFile(p2)
	return e1 == nil && e2 == nil && bytes.Equal(b1, b2)
}

func describeMode(info os.FileInfo, path string) string {
	if info.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(path); err == nil {
			return fmt.Sprintf("symlink -> %s", target)
		}
		return "symlink"
	}
	return info.Mode().String()
}

func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	_ = os.Remove(dst)
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func simpleUnifiedDiff(nameA, nameB, a, b string) string {
	if a == b {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", nameA, nameB)
	for _, line := range strings.Split(strings.TrimSuffix(a, "\n"), "\n") {
		if a != "" {
			sb.WriteString("-" + line + "\n")
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(b, "\n"), "\n") {
		if b != "" {
			sb.WriteString("+" + line + "\n")
		}
	}
	return sb.String()
}

func normalizeRoot(root string) string {
	if strings.TrimSpace(root) == "" {
		return "/"
	}
	return root
}
