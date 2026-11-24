package sync

import (
	"fmt"
	"io"
	stdsync "sync"
	"time"
)

// Printer provides mutex-synchronized, non-interleaved terminal output.
type Printer struct {
	mu  stdsync.Mutex
	out io.Writer
	err io.Writer
}

// NewPrinter constructs a synchronized terminal printer.
func NewPrinter(out, errOut io.Writer) *Printer {
	return &Printer{
		out: out,
		err: errOut,
	}
}

// Sync emits a [SYNC] status line when starting a collection sync.
func (p *Printer) Sync(collection, proto, dest string) {
	p.printf(p.out, "[SYNC] %-14s (%-7s) -> %s\n", collection, proto, dest)
}

// Done emits a [DONE] status line with elapsed duration and summary detail.
func (p *Printer) Done(collection, proto string, elapsed time.Duration, detail string) {
	if detail != "" {
		p.printf(p.out, "[DONE] %-14s (%-7s) %7s  %s\n", collection, proto, formatDuration(elapsed), detail)
		return
	}
	p.printf(p.out, "[DONE] %-14s (%-7s) %7s\n", collection, proto, formatDuration(elapsed))
}

// Skip emits a [SKIP] status line when no changes were needed.
func (p *Printer) Skip(collection, proto string, elapsed time.Duration, reason string) {
	if reason == "" {
		reason = "up-to-date"
	}
	p.printf(p.out, "[SKIP] %-14s (%-7s) %7s  %s\n", collection, proto, formatDuration(elapsed), reason)
}

// Fail emits a [FAIL] status line with elapsed duration and actionable error.
func (p *Printer) Fail(collection, proto string, elapsed time.Duration, errMsg string) {
	p.printf(p.err, "[FAIL] %-14s (%-7s) %7s  %s\n", collection, proto, formatDuration(elapsed), errMsg)
}

// Info emits a general informational line synchronized with worker output.
func (p *Printer) Info(format string, args ...any) {
	p.printf(p.out, format+"\n", args...)
}

func (p *Printer) printf(w io.Writer, format string, args ...any) {
	if p == nil || w == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(w, format, args...)
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}
