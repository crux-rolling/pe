# pe

`pe` is a standalone Go package manager for standard CRUX Linux installations. It adapts CRUX's `pkgutils` package database, locking, `/etc/pkgadd.conf` policy, and `Pkgfile`/`pkgmk` build conventions into a single binary-first CLI.

## Design Principles

- **CRUX-native database compatibility**: Reads and atomically writes `/var/lib/pkg/db` with `flock` on `/var/lib/pkg`, `.incomplete_transaction` + `.backup` commits, `/etc/pkgadd.conf` (`INSTALL`/`UPGRADE`) rules, `/var/lib/pkg/rejected/` handling, conflict detection, shared-file-safe removal, and `/sbin/ldconfig`.
- **Standard CRUX archive naming**: Supports `<name>#<version>-<release>.pkg.tar.<ext>` across `zst` (default), `xz`, `gz`, `bz2`, and `lz`.
- **Strictly binary-first installs**: `pe install` consumes `index.json` from `kryx-linux/ports` release tag `rolling-x86_64`. When no binary exists, `pe` never auto-builds; it reports the missing binary and directs you to run `pe build <pkg>`.
- **Ports stay in place**: Uses your existing CRUX ports tree (`/usr/ports` by default) without relocating ports.
- **Manual INI configuration**: Build flags (`jobs`, `cflags`, `cxxflags`, `makeflags`, `compression`), source cache, work/package directories, and repo URLs are configured in `/etc/pe.ini`.

## Building & Installing

```sh
make verify
sudo make install
```

This installs `/usr/bin/pe`, `/usr/bin/pe-index`, and `/etc/pe.ini` (if not already present).

## Usage

```sh
# Sync binary repository index.json (add --ports to also run /etc/ports/drivers)
pe sync
pe sync --ports

# Search (short-form output across binaries, ports, and installed packages)
pe search zlib

# Install strictly binary-first (or install a local .pkg.tar.<ext> archive)
pe install zlib
pe install --deps git

# Build explicitly from your local CRUX ports tree using pe.ini settings
pe build foo
pe build --install foo

# Upgrade installed packages binary-first
pe update
pe update --dry-run

# List installed packages, local ports, available binaries, package files, or file owners
pe list
pe list --ports
pe list --binaries
pe list --files zlib
pe list --owner 'usr/bin/git$'
pe list --diff

# Remove installed packages using CRUX pkgrm policy
pe remove foo
pe uninstall foo
```

## Configuration (`/etc/pe.ini`)

See [pe.ini.example](pe.ini.example) for all supported `[paths]`, `[repo]`, and `[build]` options. Override the config file with `--config /path/to/pe.ini` or `PE_CONFIG=/path/to/pe.ini`.

## Repository Indexing & GitHub Actions

Binary repository releases and `index.json` updates for `kryx-linux/ports` (`rolling-x86_64`) are automated via `.github/workflows/release-index.yml` using `pe-index`.
