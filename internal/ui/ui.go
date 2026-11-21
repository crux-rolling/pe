// Package ui provides TTY-aware ANSI color styling and download progress logging.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiCyan   = "\x1b[36m"
)

// NormalizeColorMode validates and normalizes a color mode string.
func NormalizeColorMode(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ColorAuto:
		return ColorAuto, nil
	case ColorAlways, "yes", "true", "1":
		return ColorAlways, nil
	case ColorNever, "no", "false", "0":
		return ColorNever, nil
	default:
		return "", fmt.Errorf("invalid color mode %q (expected auto, always, or never)", mode)
	}
}

// IsTTY reports whether w is backed by an interactive terminal file descriptor.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TCGETS),
		uintptr(unsafe.Pointer(&termios)),
	)
	return errno == 0
}

// ShouldColor determines whether ANSI escape codes should be emitted to w.
func ShouldColor(mode string, w io.Writer) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	default:
		if os.Getenv("NO_COLOR") != "" {
			return false
		}
		if strings.EqualFold(os.Getenv("TERM"), "dumb") {
			return false
		}
		return IsTTY(w)
	}
}

// Styler formats strings with optional ANSI color codes.
type Styler struct {
	Enabled bool
	TTY     bool
}

// NewStyler constructs a Styler for writer w using mode (auto|always|never).
func NewStyler(mode string, w io.Writer) Styler {
	return Styler{
		Enabled: ShouldColor(mode, w),
		TTY:     IsTTY(w),
	}
}

func (s Styler) wrap(code, text string) string {
	if !s.Enabled || text == "" {
		return text
	}
	return code + text + ansiReset
}

func (s Styler) Bold(text string) string   { return s.wrap(ansiBold, text) }
func (s Styler) Dim(text string) string    { return s.wrap(ansiDim, text) }
func (s Styler) Red(text string) string    { return s.wrap(ansiRed, text) }
func (s Styler) Green(text string) string  { return s.wrap(ansiGreen, text) }
func (s Styler) Yellow(text string) string { return s.wrap(ansiYellow, text) }
func (s Styler) Blue(text string) string   { return s.wrap(ansiBlue, text) }
func (s Styler) Cyan(text string) string   { return s.wrap(ansiCyan, text) }

func (s Styler) BoldRed(text string) string    { return s.wrap(ansiBold+ansiRed, text) }
func (s Styler) BoldGreen(text string) string  { return s.wrap(ansiBold+ansiGreen, text) }
func (s Styler) BoldYellow(text string) string { return s.wrap(ansiBold+ansiYellow, text) }
func (s Styler) BoldCyan(text string) string   { return s.wrap(ansiBold+ansiCyan, text) }

// FormatBytes formats a byte count into a concise human-readable unit.
func FormatBytes(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FormatSpeed formats bytes transferred over duration d as a transfer rate.
func FormatSpeed(n int64, d time.Duration) string {
	secs := d.Seconds()
	if secs <= 0 {
		secs = 0.001
	}
	rate := float64(n) / secs
	return FormatBytes(int64(rate)) + "/s"
}

// Progress tracks and logs download progress cleanly on both TTY and non-TTY outputs.
type Progress struct {
	w         io.Writer
	styler    Styler
	label     string
	total     int64
	current   int64
	startedAt time.Time
	lastPrint time.Time
	printed   bool
}

// NewProgress creates a download progress tracker for label (e.g. archive filename).
// total may be <= 0 if Content-Length is unknown.
func NewProgress(w io.Writer, styler Styler, label string, total int64) *Progress {
	if w == nil {
		w = io.Discard
	}
	p := &Progress{
		w:         w,
		styler:    styler,
		label:     label,
		total:     total,
		startedAt: time.Now(),
	}
	if !styler.TTY {
		if total > 0 {
			fmt.Fprintf(w, "%s %s (%s)...\n", styler.BoldCyan("==>"), label, FormatBytes(total))
		} else {
			fmt.Fprintf(w, "%s %s...\n", styler.BoldCyan("==>"), label)
		}
	} else {
		p.renderTTY(false)
	}
	return p
}

// Write implements io.Writer to wrap io.Copy streams directly.
func (p *Progress) Write(b []byte) (int, error) {
	n := len(b)
	p.current += int64(n)
	if p.styler.TTY {
		now := time.Now()
		if !p.printed || now.Sub(p.lastPrint) >= 100*time.Millisecond {
			p.renderTTY(false)
			p.lastPrint = now
		}
	}
	return n, nil
}

// Finish completes the download log with total bytes, elapsed time, and transfer speed.
func (p *Progress) Finish(suffix string) {
	elapsed := time.Since(p.startedAt)
	if p.styler.TTY {
		p.renderTTY(true)
	}
	size := p.current
	if size == 0 && p.total > 0 {
		size = p.total
	}
	summary := fmt.Sprintf("%s (%s in %s, %s)",
		p.label,
		FormatBytes(size),
		formatDuration(elapsed),
		FormatSpeed(size, elapsed),
	)
	if suffix != "" {
		fmt.Fprintf(p.w, "%s %s %s\n", p.styler.BoldGreen("==>"), summary, p.styler.Green(suffix))
	} else {
		fmt.Fprintf(p.w, "%s %s\n", p.styler.BoldGreen("==>"), summary)
	}
}

// Fail clears any in-progress TTY line when a download fails mid-stream.
func (p *Progress) Fail() {
	if p.styler.TTY && p.printed {
		fmt.Fprint(p.w, "\r\x1b[2K")
		p.printed = false
	}
}

func (p *Progress) renderTTY(clearOnly bool) {
	if clearOnly {
		if p.printed {
			fmt.Fprint(p.w, "\r\x1b[2K")
			p.printed = false
		}
		return
	}
	elapsed := time.Since(p.startedAt)
	speed := FormatSpeed(p.current, elapsed)
	if p.total > 0 {
		pct := float64(p.current) * 100 / float64(p.total)
		if pct > 100 {
			pct = 100
		}
		bar := renderBar(pct, 20)
		fmt.Fprintf(p.w, "\r\x1b[2K%s %s %s %5.1f%% (%s / %s, %s)",
			p.styler.BoldCyan("==>"),
			p.label,
			p.styler.Cyan(bar),
			pct,
			FormatBytes(p.current),
			FormatBytes(p.total),
			speed,
		)
	} else {
		fmt.Fprintf(p.w, "\r\x1b[2K%s %s (%s, %s)",
			p.styler.BoldCyan("==>"),
			p.label,
			FormatBytes(p.current),
			speed,
		)
	}
	p.printed = true
}

func renderBar(pct float64, width int) string {
	filled := int(pct * float64(width) / 100.0)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
