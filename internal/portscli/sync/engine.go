// Package sync implements pure-Go, zero-shell-out synchronization of CRUX ports
// collections across native Git (go-git/v5), native Rsync daemon (gokrazy/rsync),
// native HTTPUp (net/http), and native HTTP tarballs (archive/tar + compress/gzip).
package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"time"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

// DefaultMaxWorkers is the default bounded concurrency limit for collection syncs.
const DefaultMaxWorkers = 4

// SyncOutcome describes the non-error outcome of a single collection sync.
type SyncOutcome struct {
	Skipped bool
	Detail  string
}

// Result captures the complete execution result for one collection.
type Result struct {
	Source  sources.Source
	Elapsed time.Duration
	Skipped bool
	Detail  string
	Err     error
}

// Summary aggregates results across a SyncAll invocation.
type Summary struct {
	Results []Result
	Elapsed time.Duration
	Updated int
	Skipped int
	Failed  int
}

// Engine coordinates concurrent collection sync operations.
type Engine struct {
	MaxWorkers int
	HTTPClient *http.Client
	Printer    *Printer
}

// NewEngine constructs a Sync Engine with sensible defaults.
func NewEngine(maxWorkers int, printer *Printer) *Engine {
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	return &Engine{
		MaxWorkers: maxWorkers,
		HTTPClient: NewHTTPClient(),
		Printer:    printer,
	}
}

// SyncAll synchronizes all provided collections using a bounded worker pool.
// Results are returned in the same order as the input sources.
func (e *Engine) SyncAll(ctx context.Context, srcs []sources.Source) Summary {
	startAll := time.Now()
	summary := Summary{
		Results: make([]Result, len(srcs)),
	}
	if len(srcs) == 0 {
		return summary
	}

	workers := e.MaxWorkers
	if workers <= 0 {
		workers = DefaultMaxWorkers
	}
	if len(srcs) < workers {
		workers = len(srcs)
	}

	type workItem struct {
		idx int
		src sources.Source
	}

	jobs := make(chan workItem)
	var wg stdsync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				res := e.syncOne(ctx, item.src)
				summary.Results[item.idx] = res
			}
		}()
	}

	for i, src := range srcs {
		jobs <- workItem{idx: i, src: src}
	}
	close(jobs)
	wg.Wait()

	for _, r := range summary.Results {
		switch {
		case r.Err != nil:
			summary.Failed++
		case r.Skipped:
			summary.Skipped++
		default:
			summary.Updated++
		}
	}
	summary.Elapsed = time.Since(startAll)
	return summary
}

func (e *Engine) syncOne(ctx context.Context, src sources.Source) Result {
	if ctx.Err() != nil {
		return Result{
			Source: src,
			Err:    errors.New("sync canceled"),
		}
	}

	if e.Printer != nil {
		e.Printer.Sync(src.Collection, string(src.Protocol), src.DestDir)
	}

	start := time.Now()
	var (
		outcome SyncOutcome
		err     error
	)

	switch src.Protocol {
	case sources.ProtoHTTPUp:
		outcome, err = syncHTTPUp(ctx, e.HTTPClient, src)
	case sources.ProtoGit:
		outcome, err = syncGit(ctx, src)
	case sources.ProtoRsync:
		outcome, err = syncRsync(ctx, src)
	case sources.ProtoTarball:
		outcome, err = syncTarball(ctx, e.HTTPClient, src)
	default:
		err = fmt.Errorf("unsupported protocol %q", src.Protocol)
	}

	if err == nil {
		linkNestedCollections(src.DestDir)
	}

	elapsed := time.Since(start)
	res := Result{
		Source:  src,
		Elapsed: elapsed,
		Skipped: outcome.Skipped,
		Detail:  outcome.Detail,
		Err:     err,
	}

	if e.Printer != nil {
		switch {
		case err != nil:
			e.Printer.Fail(src.Collection, string(src.Protocol), elapsed, err.Error())
		case outcome.Skipped:
			e.Printer.Skip(src.Collection, string(src.Protocol), elapsed, outcome.Detail)
		default:
			e.Printer.Done(src.Collection, string(src.Protocol), elapsed, outcome.Detail)
		}
	}
	return res
}

// CollectionState describes the local filesystem state of a configured collection.
type CollectionState struct {
	Exists    bool
	PortCount int
	State     string
}

// InspectCollection checks the local target directory for a configured collection
// and summarizes its sync status purely in Go without performing network I/O.
func InspectCollection(_ context.Context, src sources.Source) CollectionState {
	info, err := os.Stat(src.DestDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CollectionState{Exists: false, State: "not synced"}
		}
		return CollectionState{Exists: false, State: "unreadable"}
	}
	if !info.IsDir() {
		return CollectionState{Exists: true, State: "invalid (not a directory)"}
	}

	ports := countPorts(src.DestDir)
	empty, _ := isDirEmpty(src.DestDir)
	if empty {
		return CollectionState{Exists: true, PortCount: 0, State: "empty"}
	}

	if src.Protocol == sources.ProtoGit {
		gitEntry := filepath.Join(src.DestDir, ".git")
		if _, gerr := os.Stat(gitEntry); gerr != nil {
			return CollectionState{
				Exists:    true,
				PortCount: ports,
				State:     fmt.Sprintf("non-git (%d ports; will recover)", ports),
			}
		}
		if !isHealthyGitWorkTree(src.DestDir) {
			return CollectionState{
				Exists:    true,
				PortCount: ports,
				State:     fmt.Sprintf("corrupt git (%d ports; will recover)", ports),
			}
		}
		if sha := shortHEAD(src.DestDir); sha != "" {
			return CollectionState{
				Exists:    true,
				PortCount: ports,
				State:     fmt.Sprintf("synced @ %s (%d ports)", sha, ports),
			}
		}
	}

	return CollectionState{
		Exists:    true,
		PortCount: ports,
		State:     fmt.Sprintf("synced (%d ports)", ports),
	}
}

func countPorts(dir string) int {
	if n := countDirectPorts(dir); n > 0 {
		return n
	}
	// Multi-collection repo layout (e.g. kryx-linux/ports containing pkg-tools/ & kernel/).
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	total := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		total += countDirectPorts(filepath.Join(dir, entry.Name()))
	}
	return total
}

func countDirectPorts(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name(), "Pkgfile")); err == nil {
			count++
		}
	}
	return count
}

// linkNestedCollections exposes nested collection directories inside a multi-collection
// repository (such as /usr/ports/kryx/{pkg-tools,kernel}) as relative symlinks in the
// parent ports directory (/usr/ports/pkg-tools -> kryx/pkg-tools) when safe to do so.
func linkNestedCollections(destDir string) {
	if countDirectPorts(destDir) > 0 {
		return
	}
	parentDir := filepath.Dir(filepath.Clean(destDir))
	baseName := filepath.Base(filepath.Clean(destDir))
	if parentDir == "" || parentDir == "." || baseName == "" || baseName == "." {
		return
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		subName := entry.Name()
		if countDirectPorts(filepath.Join(destDir, subName)) == 0 {
			continue
		}
		linkPath := filepath.Join(parentDir, subName)
		wantTarget := filepath.Join(baseName, subName)
		if fi, err := os.Lstat(linkPath); err == nil {
			// Only update existing symlinks that already point inside baseName/.
			if fi.Mode()&os.ModeSymlink == 0 {
				continue
			}
			curTarget, rerr := os.Readlink(linkPath)
			if rerr != nil || curTarget == wantTarget || !strings.HasPrefix(curTarget, baseName+string(filepath.Separator)) {
				continue
			}
			_ = os.Remove(linkPath)
		}
		_ = os.Symlink(wantTarget, linkPath)
	}
}
