# Updating the `kpm` Version (Single Source of Truth)

`kpm` uses **one single canonical version** across the entire repository and all binaries it builds (`kpm`, `kpm-index`, `kryx-src`, and every multi-call symlink: `ports`, `pkgadd`, `pkgrm`, `pkginfo`, `pkgmk`, `rejmerge`).

## Architecture (Only One Place to Edit)

| File | Role | Needs Manual Edit? |
| :--- | :--- | :--- |
| [`VERSION`](../VERSION) | **Single source of truth** (plain text, e.g. `0.6.0`) | **Yes** (or run `./dev/set-version.sh <ver>`) |
| [`version.go`](../version.go) | Embeds `VERSION` via `//go:embed VERSION` into `kpm.Version` | No (automatic) |
| [`internal/cli/cli.go`](../internal/cli/cli.go) | `var Version = kpmroot.Version` (propagates to `kpm`, `kryx-src`, `ports`, `pkgadd`, `pkgrm`, `pkginfo`, `pkgmk`, `rejmerge`) | No (automatic) |
| [`cmd/kpm-index/main.go`](../cmd/kpm-index/main.go) | Prints `kpmroot.Version` on `-v` / `--version` | No (automatic) |
| [`Makefile`](../Makefile) | Reads `VERSION ?= $(shell tr -d '[:space:]' < VERSION)` and injects `-ldflags` | No (automatic) |
| [`iso/scripts/build-kpm.sh`](../../iso/scripts/build-kpm.sh) | Auto-reads `$KPM_SRC/VERSION` (falls back to `git describe`) | No (automatic) |

No other Go source file in `kpm` hardcodes a release version string.

## How to Bump the Version

### Option 1: Using the Script (Recommended)
```sh
# Update kpm/VERSION only:
./dev/set-version.sh 0.6.1

# Or via Makefile:
make set-version V=0.6.1

# Also update the sibling Pkgfile (../ports/kpm-tools/kpm/Pkgfile) in one command:
./dev/set-version.sh --sync-ecosystem 0.6.1

# Preview without modifying files:
./dev/set-version.sh --dry-run --sync-ecosystem 0.6.1
```

### Option 2: Direct Edit
Edit the single line in [`VERSION`](../VERSION), e.g.:
```text
0.6.1
```

## Verification & Release Checklist

1. **Run unit tests and static build:**
   ```sh
   CGO_ENABLED=0 go vet -mod=vendor ./...
   CGO_ENABLED=0 go test -mod=vendor ./...
   make build
   ```
2. **Confirm every binary reports the new version:**
   ```sh
   ./bin/kpm --version
   ./bin/kpm ports --version
   ./bin/kpm-index --version
   ./bin/kryx-src --version
   ```
3. **Commit and tag `kpm`:**
   ```sh
   git add VERSION
   git commit -m "release: v0.6.1"
   git tag -a v0.6.1 -m "kpm v0.6.1"
   git push origin main --tags
   ```
4. **Update external ecosystem packaging (if not already synced with `--sync-ecosystem`):**
   - `kryx-linux/ports`: update `version=` (and reset `release=1`) in `kpm-tools/kpm/Pkgfile`.
   - `kryx-linux/iso`: automatically picks up `VERSION` from `kryx-linux/kpm` during CI builds.
