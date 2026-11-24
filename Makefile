PREFIX ?= /usr
BINDIR ?= $(PREFIX)/bin
MANDIR ?= $(PREFIX)/share/man
BASHCOMPDIR ?= $(PREFIX)/share/bash-completion/completions
ETCDIR ?= /etc
PROFILED ?= $(ETCDIR)/profile.d
DESTDIR ?=

VERSION ?= $(shell tr -d '[:space:]' < VERSION)
LDFLAGS := -s -w -X github.com/kryx-linux/kpm.Version=$(VERSION) -X github.com/kryx-linux/kpm/internal/cli.Version=$(VERSION)
MODFLAGS ?= -mod=vendor
GO_SRCS := $(shell find cmd internal -type f -name '*.go') version.go VERSION go.mod

.PHONY: all build test verify set-version clean install uninstall clean-legacy purge

all: build

set-version:
	@test -n "$(V)" || { echo "Usage: make set-version V=<new-version> (e.g. make set-version V=0.6.1)" >&2; exit 2; }
	./dev/set-version.sh "$(V)"

build: bin/kpm bin/kpm-index bin/kryx-src

bin/kpm: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build $(MODFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kpm

bin/kpm-index: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build $(MODFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kpm-index

bin/kryx-src: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build $(MODFLAGS) -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kryx-src

test:
	CGO_ENABLED=0 go test $(MODFLAGS) -v ./...

verify: test build

CRUX_COMPAT_SYMLINKS := pkgadd pkgrm pkginfo pkgmk ports rejmerge

install: build
	install -D -m 0755 bin/kpm $(DESTDIR)$(BINDIR)/kpm
	install -D -m 0755 bin/kpm-index $(DESTDIR)$(BINDIR)/kpm-index
	install -D -m 0755 bin/kryx-src $(DESTDIR)$(BINDIR)/kryx-src
	for link in $(CRUX_COMPAT_SYMLINKS); do \
		ln -sf kpm $(DESTDIR)$(BINDIR)/$$link; \
	done
	install -D -m 0755 scripts/setup $(DESTDIR)$(BINDIR)/setup
	install -D -m 0755 scripts/setup-kryx $(DESTDIR)$(BINDIR)/setup-kryx
	install -D -m 0755 scripts/setup-chroot $(DESTDIR)$(BINDIR)/setup-chroot
	install -D -m 0755 scripts/setup-repo $(DESTDIR)$(BINDIR)/setup-repo
	install -D -m 0644 man/kpm.8 $(DESTDIR)$(MANDIR)/man8/kpm.8
	install -D -m 0644 man/kpm-index.8 $(DESTDIR)$(MANDIR)/man8/kpm-index.8
	install -D -m 0644 man/kryx-src.8 $(DESTDIR)$(MANDIR)/man8/kryx-src.8
	install -D -m 0644 man/ports.8 $(DESTDIR)$(MANDIR)/man8/ports.8
	install -D -m 0644 man/setup.8 $(DESTDIR)$(MANDIR)/man8/setup.8
	install -D -m 0644 man/setup-kryx.8 $(DESTDIR)$(MANDIR)/man8/setup-kryx.8
	install -D -m 0644 man/setup-chroot.8 $(DESTDIR)$(MANDIR)/man8/setup-chroot.8
	install -D -m 0644 man/setup-repo.8 $(DESTDIR)$(MANDIR)/man8/setup-repo.8
	install -D -m 0644 completions/bash/kpm $(DESTDIR)$(BASHCOMPDIR)/kpm
	ln -sf kpm $(DESTDIR)$(BASHCOMPDIR)/kpm-index
	install -D -m 0644 completions/bash/kpm.profile.sh $(DESTDIR)$(PROFILED)/kpm-completion.sh
	install -d -m 0755 $(DESTDIR)$(ETCDIR)/ports/sources.list.d
	if [ ! -f $(DESTDIR)$(ETCDIR)/ports/sources.list ]; then \
		install -D -m 0644 configs/sources.list $(DESTDIR)$(ETCDIR)/ports/sources.list; \
	fi
	if [ ! -f $(DESTDIR)$(ETCDIR)/kpm.ini ]; then \
		install -D -m 0644 kpm.ini.example $(DESTDIR)$(ETCDIR)/kpm.ini; \
	fi
	@if [ -z "$(DESTDIR)" ]; then \
		for stale in /sbin/kpm /sbin/kpm-index /sbin/kryx-src /usr/local/bin/kpm /usr/local/bin/kpm-index /usr/local/bin/kryx-src; do \
			base="$$(basename "$$stale")"; \
			if [ -e "$$stale" ] && [ ! "$$stale" -ef "$(BINDIR)/$$base" ]; then \
				echo "WARNING: legacy binary found at $$stale (may shadow $(BINDIR)/$$base in PATH). Run 'make clean-legacy' and 'hash -r'."; \
			fi; \
		done; \
	fi

clean-legacy:
	@for stale in $(DESTDIR)/sbin/kpm $(DESTDIR)/sbin/kpm-index $(DESTDIR)/sbin/kryx-src $(DESTDIR)/usr/local/bin/kpm $(DESTDIR)/usr/local/bin/kpm-index $(DESTDIR)/usr/local/bin/kryx-src; do \
		base="$$(basename "$$stale")"; \
		if [ -e "$$stale" ] && [ ! "$$stale" -ef "$(DESTDIR)$(BINDIR)/$$base" ]; then \
			rm -f "$$stale"; \
		fi; \
	done
	@echo "Legacy binaries removed. Run 'hash -r' in your shell to clear cached command paths."

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/kpm \
	      $(DESTDIR)$(BINDIR)/kpm-index \
	      $(DESTDIR)$(BINDIR)/kryx-src \
	      $(DESTDIR)$(BINDIR)/setup \
	      $(DESTDIR)$(BINDIR)/setup-kryx \
	      $(DESTDIR)$(BINDIR)/setup-chroot \
	      $(DESTDIR)$(BINDIR)/setup-repo \
	      $(DESTDIR)$(MANDIR)/man8/kpm.8 \
	      $(DESTDIR)$(MANDIR)/man8/kpm-index.8 \
	      $(DESTDIR)$(MANDIR)/man8/kryx-src.8 \
	      $(DESTDIR)$(MANDIR)/man8/ports.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup-kryx.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup-chroot.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup-repo.8 \
	      $(DESTDIR)$(BASHCOMPDIR)/kpm \
	      $(DESTDIR)$(BASHCOMPDIR)/kpm-index \
	      $(DESTDIR)$(PROFILED)/kpm-completion.sh
	for link in $(CRUX_COMPAT_SYMLINKS); do \
		if [ -L $(DESTDIR)$(BINDIR)/$$link ] && [ "$$(readlink $(DESTDIR)$(BINDIR)/$$link)" = "kpm" ]; then \
			rm -f $(DESTDIR)$(BINDIR)/$$link; \
		fi; \
	done
	@echo "Uninstalled kpm tools. Run 'make clean-legacy' if older /sbin/kpm exists, then 'hash -r'."

purge: uninstall clean-legacy
	rm -rf $(DESTDIR)$(ETCDIR)/kpm.ini \
	       $(DESTDIR)$(ETCDIR)/kpm \
	       $(DESTDIR)/var/cache/kpm \
	       $(DESTDIR)/var/lib/kpm

clean:
	rm -rf bin
