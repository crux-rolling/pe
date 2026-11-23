PREFIX ?= /usr
BINDIR ?= $(PREFIX)/bin
MANDIR ?= $(PREFIX)/share/man
BASHCOMPDIR ?= $(PREFIX)/share/bash-completion/completions
ETCDIR ?= /etc
PROFILED ?= $(ETCDIR)/profile.d
DESTDIR ?=

VERSION ?= 0.5.2
LDFLAGS := -s -w -X github.com/kryx-linux/pe/internal/cli.Version=$(VERSION)
GO_SRCS := $(shell find cmd internal -type f -name '*.go') go.mod

.PHONY: all build test verify clean install uninstall clean-legacy purge

all: build

build: bin/pe bin/pe-index bin/kryx-src

bin/pe: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/pe

bin/pe-index: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/pe-index

bin/kryx-src: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/kryx-src

test:
	go test -v ./...

verify: test build

install: build
	install -D -m 0755 bin/pe $(DESTDIR)$(BINDIR)/pe
	install -D -m 0755 bin/pe-index $(DESTDIR)$(BINDIR)/pe-index
	install -D -m 0755 bin/kryx-src $(DESTDIR)$(BINDIR)/kryx-src
	install -D -m 0755 scripts/setup $(DESTDIR)$(BINDIR)/setup
	install -D -m 0755 scripts/setup-kryx $(DESTDIR)$(BINDIR)/setup-kryx
	install -D -m 0755 scripts/setup-chroot $(DESTDIR)$(BINDIR)/setup-chroot
	install -D -m 0755 scripts/setup-repo $(DESTDIR)$(BINDIR)/setup-repo
	install -D -m 0644 man/pe.8 $(DESTDIR)$(MANDIR)/man8/pe.8
	install -D -m 0644 man/pe-index.8 $(DESTDIR)$(MANDIR)/man8/pe-index.8
	install -D -m 0644 man/kryx-src.8 $(DESTDIR)$(MANDIR)/man8/kryx-src.8
	install -D -m 0644 man/setup.8 $(DESTDIR)$(MANDIR)/man8/setup.8
	install -D -m 0644 man/setup-kryx.8 $(DESTDIR)$(MANDIR)/man8/setup-kryx.8
	install -D -m 0644 man/setup-chroot.8 $(DESTDIR)$(MANDIR)/man8/setup-chroot.8
	install -D -m 0644 man/setup-repo.8 $(DESTDIR)$(MANDIR)/man8/setup-repo.8
	install -D -m 0644 completions/bash/pe $(DESTDIR)$(BASHCOMPDIR)/pe
	ln -sf pe $(DESTDIR)$(BASHCOMPDIR)/pe-index
	install -D -m 0644 completions/bash/pe.profile.sh $(DESTDIR)$(PROFILED)/pe-completion.sh
	if [ ! -f $(DESTDIR)$(ETCDIR)/pe.ini ]; then \
		install -D -m 0644 pe.ini.example $(DESTDIR)$(ETCDIR)/pe.ini; \
	fi
	@if [ -z "$(DESTDIR)" ]; then \
		for stale in /sbin/pe /sbin/pe-index /sbin/kryx-src /usr/local/bin/pe /usr/local/bin/pe-index /usr/local/bin/kryx-src; do \
			if [ -e "$$stale" ]; then \
				echo "WARNING: legacy binary found at $$stale (may shadow $(BINDIR)/pe in PATH). Run 'make clean-legacy' and 'hash -r'."; \
			fi; \
		done; \
	fi

clean-legacy:
	rm -f $(DESTDIR)/sbin/pe \
	      $(DESTDIR)/sbin/pe-index \
	      $(DESTDIR)/sbin/kryx-src \
	      $(DESTDIR)/usr/local/bin/pe \
	      $(DESTDIR)/usr/local/bin/pe-index \
	      $(DESTDIR)/usr/local/bin/kryx-src
	@echo "Legacy binaries removed. Run 'hash -r' in your shell to clear cached command paths."

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/pe \
	      $(DESTDIR)$(BINDIR)/pe-index \
	      $(DESTDIR)$(BINDIR)/kryx-src \
	      $(DESTDIR)$(BINDIR)/setup \
	      $(DESTDIR)$(BINDIR)/setup-kryx \
	      $(DESTDIR)$(BINDIR)/setup-chroot \
	      $(DESTDIR)$(BINDIR)/setup-repo \
	      $(DESTDIR)$(MANDIR)/man8/pe.8 \
	      $(DESTDIR)$(MANDIR)/man8/pe-index.8 \
	      $(DESTDIR)$(MANDIR)/man8/kryx-src.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup-kryx.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup-chroot.8 \
	      $(DESTDIR)$(MANDIR)/man8/setup-repo.8 \
	      $(DESTDIR)$(BASHCOMPDIR)/pe \
	      $(DESTDIR)$(BASHCOMPDIR)/pe-index \
	      $(DESTDIR)$(PROFILED)/pe-completion.sh
	@echo "Uninstalled pe tools. Run 'make clean-legacy' if older /sbin/pe exists, then 'hash -r'."

purge: uninstall clean-legacy
	rm -rf $(DESTDIR)$(ETCDIR)/pe.ini \
	       $(DESTDIR)$(ETCDIR)/pe \
	       $(DESTDIR)/var/cache/pe

clean:
	rm -rf bin
