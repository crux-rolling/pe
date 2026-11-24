#!/usr/bin/env bash
# dev/set-version.sh - Update the single canonical version for the entire kpm toolchain
# (`kpm`, `kpm-index`, `kryx-src`, `ports`, `pkgadd`, `pkgrm`, `pkginfo`, `pkgmk`, `rejmerge`).
#
# Because every Go binary and the Makefile derive their version directly from the root
# `VERSION` file (`//go:embed VERSION` in version.go + `$(shell cat VERSION)` in Makefile),
# updating `VERSION` updates every binary in `kpm` in one place.
#
# Optionally pass `--sync-ecosystem` to also bump sibling `../ports/pkg-tools/kpm/Pkgfile`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${REPO_ROOT}/VERSION"

SYNC_ECOSYSTEM=0
DRY_RUN=0
NEW_VERSION=""

usage() {
    cat <<'EOF'
Usage: ./dev/set-version.sh [options] <new-version>

Sets the single canonical version in kpm/VERSION (embedded into kpm, kpm-index,
kryx-src, and integrated ports via version.go + Makefile).

Options:
  --sync-ecosystem   Also update sibling ../ports/pkg-tools/kpm/Pkgfile (if present)
  --dry-run          Print what would change without modifying files
  -h, --help         Show this help message

Examples:
  ./dev/set-version.sh 0.6.1
  ./dev/set-version.sh --sync-ecosystem 0.7.0
  make set-version V=0.6.1
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --sync-ecosystem) SYNC_ECOSYSTEM=1; shift ;;
        --dry-run)        DRY_RUN=1; shift ;;
        -h|--help)        usage; exit 0 ;;
        -*)               echo "set-version.sh: unknown option: $1" >&2; usage >&2; exit 2 ;;
        *)
            if [[ -n "$NEW_VERSION" ]]; then
                echo "set-version.sh: expected a single version argument" >&2
                exit 2
            fi
            NEW_VERSION="$1"
            shift
            ;;
    esac
done

if [[ -z "$NEW_VERSION" ]]; then
    usage >&2
    exit 2
fi

# Strip optional leading 'v' (e.g. v0.6.1 -> 0.6.1) and validate SemVer-compatible string.
NEW_VERSION="${NEW_VERSION#v}"
if [[ ! "$NEW_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.-]+)?$ ]]; then
    echo "set-version.sh: error: invalid version '${NEW_VERSION}' (expected e.g. 0.6.1 or 0.7.0-rc1)" >&2
    exit 2
fi

OLD_VERSION="$(tr -d '[:space:]' < "$VERSION_FILE" 2>/dev/null || echo 'unset')"
echo "[set-version] kpm version: ${OLD_VERSION} -> ${NEW_VERSION}"

if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "[dry-run] Would write '${NEW_VERSION}' to ${VERSION_FILE}"
else
    printf '%s\n' "$NEW_VERSION" > "$VERSION_FILE"
    echo "  [OK] Updated ${VERSION_FILE}"
fi

# Optional sibling ecosystem sync (ports/pkg-tools/kpm/Pkgfile)
PKGFILE="${REPO_ROOT}/../ports/pkg-tools/kpm/Pkgfile"
if [[ "$SYNC_ECOSYSTEM" -eq 1 ]]; then
    if [[ -f "$PKGFILE" ]]; then
        if [[ "$DRY_RUN" -eq 1 ]]; then
            echo "[dry-run] Would update version=${NEW_VERSION} in ${PKGFILE}"
        else
            sed -i -E "s|^version=.*|version=${NEW_VERSION}|" "$PKGFILE"
            echo "  [OK] Updated ${PKGFILE}"
        fi
    else
        echo "  [WARN] Sibling Pkgfile not found at ${PKGFILE}" >&2
    fi
elif [[ -f "$PKGFILE" ]]; then
    echo "  [NOTE] Sibling Pkgfile exists at ${PKGFILE} (re-run with --sync-ecosystem to update it too)."
fi

echo ""
echo "Next steps:"
echo "  1. Verify & build:  CGO_ENABLED=0 go test -mod=vendor ./... && make build"
echo "  2. Commit & tag:    git commit -am \"release: v${NEW_VERSION}\" && git tag -a \"v${NEW_VERSION}\" -m \"kpm v${NEW_VERSION}\""
