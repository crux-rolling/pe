# `kpm` (KryX Package Manager)

`kpm` is a standalone Go package manager for CRUX / KryX Linux installations with 100% upstream CRUX `pkgutils` and `ports` parity. It provides a unified binary-first CLI alongside multi-call symlink compatibility for `pkgadd`, `pkgrm`, `pkginfo`, `pkgmk`, `ports`, and `rejmerge`.

## Design Principles

- **CRUX-native database compatibility**: Reads and atomically writes `/var/lib/pkg/db` with `flock` on `/var/lib/pkg`, `.incomplete_transaction` + `.backup` commits, `/etc/pkgadd.conf` (`INSTALL`/`UPGRADE`) rules, `/var/lib/pkg/rejected/` handling, conflict detection, shared-file-safe removal, and `/sbin/ldconfig`.
- **100% CRUX multi-call parity**: Symlinking `pkgadd`, `pkgrm`, `pkginfo`, `pkgmk`, `ports`, and `rejmerge` to `kpm` translates every upstream flag, config directive, environment variable, and exit code 1:1.
- **Standard CRUX archive naming**: Supports `<name>#<version>-<release>.pkg.tar.<ext>` across `zst` (default), `xz`, `gz`, `bz2`, and `lz`.
- **Strictly binary-first installs**: `kpm install` consumes `index.json` from `kryx-linux/ports` release tag `rolling-x86_64`. When no binary exists, `kpm` never auto-builds; it reports the missing binary and directs you to run `kpm build <pkg>`.
- **Ports stay in place**: Uses your existing CRUX ports tree (`/usr/ports` by default) without relocating ports.
- **Manual INI configuration**: Build flags (`jobs`, `cflags`, `cxxflags`, `makeflags`, `compression`), source cache, work/package directories, held packages, signing verification, and repo URLs are configured in `/etc/kpm.ini`.

## Building & Installing

```sh
make verify
sudo make install
```

This installs `/usr/bin/kpm`, `/usr/bin/kpm-index`, `/usr/bin/kryx-src`, CRUX compatibility symlinks (`pkgadd`, `pkgrm`, `pkginfo`, `pkgmk`, `ports`, `rejmerge` -> `kpm`), man pages, completions, and `/etc/kpm.ini` (if not already present).

## Usage

```sh
# Sync binary repository index.json (add --ports to also run /etc/ports/drivers)
kpm sync
kpm sync --ports

# Search (short-form output across binaries, ports, and installed packages)
kpm search zlib
kpm info zlib

# Install strictly binary-first (or a specific version, or a local .pkg.tar.<ext> archive)
kpm install zlib
kpm install --deps git
kpm install zlib=1.3.1-1

# Build explicitly from your local CRUX ports tree using kpm.ini settings
kpm build foo
kpm build --install foo

# Upgrade installed packages binary-first, pin/hold, or roll back
kpm update
kpm update --dry-run
kpm hold linux
kpm unhold linux
kpm downgrade foo

# List installed packages, local ports, available binaries, package files, or file owners
kpm list
kpm list --ports
kpm list --binaries
kpm list --files zlib
kpm list --owner 'usr/bin/git$'
kpm list --diff

# Remove packages, prune orphaned deps, clean cache, verify system/ABI health, merge rejected configs
kpm remove foo
kpm autoremove --dry-run
kpm clean --keep 2
kpm check --broken
kpm check --files
kpm rejected
```

## Configuration (`/etc/kpm.ini`)

See [kpm.ini.example](kpm.ini.example) for all supported `[paths]`, `[repo]`, `[build]`, and `[ui]` options. Override the config file with `--config /path/to/kpm.ini` or `KPM_CONFIG=/path/to/kpm.ini`.

## Repository Indexing & GitHub Actions

Binary repository releases, `signify`-compatible index signatures, and `index.json` updates for `kryx-linux/ports` (`rolling-x86_64`) are automated via `kpm-index` and `kryx-src`.
