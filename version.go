// Package kpm exposes the single canonical version for all binaries in this
// repository (kpm, kpm-index, kryx-src, and integrated ports).
package kpm

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var embeddedVersion string

// Version is the single source of truth for the kpm toolchain version,
// embedded directly from the repository root VERSION file and optionally
// overridable at link time via -ldflags "-X github.com/kryx-linux/kpm.Version=...".
var Version = strings.TrimSpace(embeddedVersion)
