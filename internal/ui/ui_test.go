package ui

import (
	"bytes"
	"strings"
	"testing"
)

func TestColorModesAndStyler(t *testing.T) {
	var buf bytes.Buffer

	// Auto mode on non-TTY buffer disables color.
	auto := NewStyler(ColorAuto, &buf)
	if auto.Enabled {
		t.Fatalf("expected auto mode on bytes.Buffer to disable color")
	}
	if got := auto.BoldGreen("ok"); got != "ok" {
		t.Fatalf("expected plain text when disabled, got %q", got)
	}

	// Always mode enables ANSI color even on non-TTY buffer.
	always := NewStyler(ColorAlways, &buf)
	if !always.Enabled {
		t.Fatalf("expected always mode to enable color")
	}
	if got := always.BoldGreen("ok"); !strings.Contains(got, "\x1b[") || !strings.Contains(got, "ok") {
		t.Fatalf("expected ANSI styled text, got %q", got)
	}

	// Never mode disables color.
	never := NewStyler(ColorNever, &buf)
	if never.Enabled {
		t.Fatalf("expected never mode to disable color")
	}

	// NO_COLOR disables color in auto mode.
	t.Setenv("NO_COLOR", "1")
	if ShouldColor(ColorAuto, &buf) {
		t.Fatalf("expected NO_COLOR to disable auto color")
	}
}

func TestProgressLogAndFormatBytes(t *testing.T) {
	if got := FormatBytes(512); got != "512 B" {
		t.Fatalf("FormatBytes(512) = %q", got)
	}
	if got := FormatBytes(2 * 1024 * 1024); got != "2.00 MiB" {
		t.Fatalf("FormatBytes(2MiB) = %q", got)
	}

	var buf bytes.Buffer
	styler := NewStyler(ColorNever, &buf)
	p := NewProgress(&buf, styler, "zig#0.17.1-1.pkg.tar.zst", 1024)
	_, _ = p.Write(make([]byte, 1024))
	p.Finish("[sha256 ok]")

	out := buf.String()
	if !strings.Contains(out, "zig#0.17.1-1.pkg.tar.zst (1.0 KiB)...") {
		t.Fatalf("expected initial download log, got %q", out)
	}
	if !strings.Contains(out, "[sha256 ok]") {
		t.Fatalf("expected completion suffix, got %q", out)
	}
}
