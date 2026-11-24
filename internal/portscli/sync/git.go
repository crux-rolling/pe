package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

// syncGit synchronizes a Git-based port collection natively in pure Go using go-git/v5
// (zero os/exec calls), supporting shallow clone/fetch and automatic recovery on
// corrupted or non-git target directories.
func syncGit(ctx context.Context, src sources.Source) (SyncOutcome, error) {
	info, statErr := os.Stat(src.DestDir)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		if err := cloneFreshGit(ctx, src.URL, src.DestDir); err != nil {
			return SyncOutcome{}, err
		}
		return SyncOutcome{Detail: formatGitDetail("cloned", shortHEAD(src.DestDir))}, nil

	case statErr != nil:
		return SyncOutcome{}, fmt.Errorf("cannot inspect destination %s: %w", src.DestDir, statErr)

	case !info.IsDir():
		if err := recoverGitDir(ctx, src.URL, src.DestDir); err != nil {
			return SyncOutcome{}, err
		}
		return SyncOutcome{Detail: formatGitDetail("recovered", shortHEAD(src.DestDir))}, nil
	}

	empty, err := isDirEmpty(src.DestDir)
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("cannot read destination %s: %w", src.DestDir, err)
	}
	if empty {
		if err := cloneFreshGit(ctx, src.URL, src.DestDir); err != nil {
			return SyncOutcome{}, err
		}
		return SyncOutcome{Detail: formatGitDetail("cloned", shortHEAD(src.DestDir))}, nil
	}

	if !isHealthyGitWorkTree(src.DestDir) {
		if err := recoverGitDir(ctx, src.URL, src.DestDir); err != nil {
			return SyncOutcome{}, err
		}
		return SyncOutcome{Detail: formatGitDetail("recovered", shortHEAD(src.DestDir))}, nil
	}

	outcome, err := updateExistingGit(ctx, src.URL, src.DestDir)
	if err == nil {
		return outcome, nil
	}
	if ctx.Err() != nil {
		return SyncOutcome{}, errors.New("sync canceled")
	}

	// Automatic staging-clone-first recovery if local .git state was inconsistent/corrupted.
	if recErr := recoverGitDir(ctx, src.URL, src.DestDir); recErr != nil {
		return SyncOutcome{}, recErr
	}
	return SyncOutcome{Detail: formatGitDetail("recovered", shortHEAD(src.DestDir))}, nil
}

func cloneFreshGit(ctx context.Context, repoURL, destDir string) error {
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("cannot create parent directory for %s: %w", destDir, err)
	}
	_, err := gogit.PlainCloneContext(ctx, destDir, false, &gogit.CloneOptions{
		URL:          repoURL,
		Depth:        1,
		SingleBranch: true,
		Tags:         gogit.NoTags,
	})
	if err != nil {
		return translateGitError(ctx, err, repoURL)
	}
	return nil
}

func isHealthyGitWorkTree(destDir string) bool {
	if _, err := os.Stat(filepath.Join(destDir, ".git")); err != nil {
		return false
	}
	repo, err := gogit.PlainOpen(destDir)
	if err != nil {
		return false
	}
	head, err := repo.Head()
	if err != nil {
		return false
	}
	if _, err := repo.CommitObject(head.Hash()); err != nil {
		return false
	}
	wt, err := repo.Worktree()
	if err != nil || wt == nil {
		return false
	}
	// Verify index is parseable (catches corrupted .git/index).
	if _, err := repo.Storer.Index(); err != nil {
		return false
	}
	return true
}

func updateExistingGit(ctx context.Context, repoURL, destDir string) (SyncOutcome, error) {
	// Remove stale lock files left by an interrupted process.
	_ = os.Remove(filepath.Join(destDir, ".git", "index.lock"))
	_ = os.Remove(filepath.Join(destDir, ".git", "shallow.lock"))

	repo, err := gogit.PlainOpen(destDir)
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("open git repo: %w", err)
	}

	oldHead, err := repo.Head()
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("read HEAD: %w", err)
	}
	oldHash := oldHead.Hash()

	// Ensure origin remote exists and matches configured URL.
	remote, rerr := repo.Remote("origin")
	if rerr != nil {
		if _, cerr := repo.CreateRemote(&gitcfg.RemoteConfig{
			Name: "origin",
			URLs: []string{repoURL},
		}); cerr != nil {
			return SyncOutcome{}, translateGitError(ctx, cerr, repoURL)
		}
	} else {
		urls := remote.Config().URLs
		if len(urls) == 0 || urls[0] != repoURL {
			_ = repo.DeleteRemote("origin")
			if _, cerr := repo.CreateRemote(&gitcfg.RemoteConfig{
				Name: "origin",
				URLs: []string{repoURL},
			}); cerr != nil {
				return SyncOutcome{}, translateGitError(ctx, cerr, repoURL)
			}
		}
	}

	// Shallow fetch from origin into refs/remotes/origin/*.
	fetchErr := repo.FetchContext(ctx, &gogit.FetchOptions{
		RemoteName: "origin",
		Depth:      1,
		Tags:       gogit.NoTags,
		Force:      true,
		RefSpecs: []gitcfg.RefSpec{
			"+refs/heads/*:refs/remotes/origin/*",
		},
	})
	if fetchErr != nil && !errors.Is(fetchErr, gogit.NoErrAlreadyUpToDate) {
		return SyncOutcome{}, translateGitError(ctx, fetchErr, repoURL)
	}

	// Resolve target commit hash for the current branch (or remote HEAD).
	targetHash, ok := resolveRemoteTargetHash(repo, oldHead)
	if !ok {
		return SyncOutcome{}, fmt.Errorf("could not resolve remote target ref for %s", repoURL)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("open worktree: %w", err)
	}

	if targetHash == oldHash {
		// Ensure local working tree is clean even when HEAD matches.
		if err := wt.Reset(&gogit.ResetOptions{Commit: targetHash, Mode: gogit.HardReset}); err != nil {
			return SyncOutcome{}, err
		}
		_ = wt.Clean(&gogit.CleanOptions{Dir: true})
		return SyncOutcome{Skipped: true, Detail: "up-to-date"}, nil
	}

	if err := wt.Reset(&gogit.ResetOptions{
		Commit: targetHash,
		Mode:   gogit.HardReset,
	}); err != nil {
		return SyncOutcome{}, translateGitError(ctx, err, repoURL)
	}
	if err := wt.Clean(&gogit.CleanOptions{Dir: true}); err != nil {
		return SyncOutcome{}, translateGitError(ctx, err, repoURL)
	}

	return SyncOutcome{Detail: formatGitDetail("updated", shortSHA(targetHash.String()))}, nil
}

func resolveRemoteTargetHash(repo *gogit.Repository, head *plumbing.Reference) (plumbing.Hash, bool) {
	if head != nil && head.Name().IsBranch() {
		branchName := head.Name().Short()
		if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true); err == nil {
			return ref.Hash(), true
		}
	}
	if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), true); err == nil {
		return ref.Hash(), true
	}
	for _, fallback := range []string{"main", "master"} {
		if ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", fallback), true); err == nil {
			return ref.Hash(), true
		}
	}
	// Last resort: pick the first remote tracking ref under refs/remotes/origin/.
	iter, err := repo.References()
	if err == nil {
		var found plumbing.Hash
		var has bool
		_ = iter.ForEach(func(ref *plumbing.Reference) error {
			if !has && ref.Name().IsRemote() && strings.HasPrefix(ref.Name().String(), "refs/remotes/origin/") {
				found = ref.Hash()
				has = true
			}
			return nil
		})
		if has {
			return found, true
		}
	}
	if head != nil {
		return head.Hash(), true
	}
	return plumbing.ZeroHash, false
}

// recoverGitDir cleanly replaces a non-git or corrupted git directory via a
// fresh shallow clone into a sibling staging directory first, so an unreachable
// remote never destroys existing local ports.
func recoverGitDir(ctx context.Context, repoURL, destDir string) error {
	parent := filepath.Dir(destDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("cannot create parent directory %s: %w", parent, err)
	}

	stageDir, err := os.MkdirTemp(parent, ".ports-git-stage-*")
	if err == nil {
		defer os.RemoveAll(stageDir)

		if cloneErr := cloneFreshGit(ctx, repoURL, stageDir); cloneErr != nil {
			return cloneErr
		}

		backupPath := destDir + ".ports-old"
		_ = os.RemoveAll(backupPath)

		if renameErr := os.Rename(destDir, backupPath); renameErr == nil {
			if swapErr := os.Rename(stageDir, destDir); swapErr == nil {
				_ = os.RemoveAll(backupPath)
				return nil
			}
			_ = os.Rename(backupPath, destDir)
		}
	}

	// Fallback (e.g., destDir is a mount point): wipe contents and clone in place.
	if info, lerr := os.Lstat(destDir); lerr == nil && !info.IsDir() {
		if rmErr := os.Remove(destDir); rmErr != nil {
			return fmt.Errorf("cannot remove non-directory %s: %w", destDir, rmErr)
		}
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("cannot create directory %s: %w", destDir, err)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return fmt.Errorf("cannot read directory %s during recovery: %w", destDir, err)
	}
	for _, e := range entries {
		if rmErr := os.RemoveAll(filepath.Join(destDir, e.Name())); rmErr != nil {
			return fmt.Errorf("cannot clean %s during recovery: %w", e.Name(), rmErr)
		}
	}
	return cloneFreshGit(ctx, repoURL, destDir)
}

func shortHEAD(destDir string) string {
	repo, err := gogit.PlainOpen(destDir)
	if err != nil {
		return ""
	}
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	return shortSHA(head.Hash().String())
}

func shortSHA(full string) string {
	if len(full) > 7 {
		return full[:7]
	}
	return full
}

func formatGitDetail(verb, sha string) string {
	if sha != "" {
		return fmt.Sprintf("%s @ %s", verb, sha)
	}
	return verb
}

func isDirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func translateGitError(ctx context.Context, err error, repoURL string) error {
	if ctx.Err() != nil {
		return errors.New("sync canceled")
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "temporary failure in name resolution"):
		return fmt.Errorf("DNS resolution failed for %s: check /etc/resolv.conf or network link", repoURL)
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "i/o timeout"):
		return fmt.Errorf("unable to connect to %s: check network link or firewall", repoURL)
	case strings.Contains(msg, "repository not found"),
		strings.Contains(msg, "404"):
		return fmt.Errorf("remote git repository not found: %s", repoURL)
	case strings.Contains(msg, "authentication required"),
		strings.Contains(msg, "authorization failed"):
		return fmt.Errorf("authentication required or denied for %s", repoURL)
	case strings.Contains(msg, "x509:"),
		strings.Contains(msg, "certificate"):
		return fmt.Errorf("TLS certificate verification failed for %s: check system CA certificates", repoURL)
	}
	return fmt.Errorf("git sync failed for %s: %w", repoURL, err)
}
