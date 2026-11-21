PREFIX ?= /usr
BINDIR ?= $(PREFIX)/bin
MANDIR ?= $(PREFIX)/share/man
BASHCOMPDIR ?= $(PREFIX)/share/bash-completion/completions
ETCDIR ?= /etc
PROFILED ?= $(ETCDIR)/profile.d
DESTDIR ?=

VERSION ?= 0.1.0
LDFLAGS := -s -w -X github.com/ezke5/pe/internal/cli.Version=$(VERSION)
GO_SRCS := $(shell find cmd internal -type f -name '*.go') go.mod

.PHONY: all build test verify clean install uninstall

all: build

build: bin/pe bin/pe-index

bin/pe: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/pe

bin/pe-index: $(GO_SRCS)
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $@ ./cmd/pe-index

test:
	go test -v ./...

verify: test build

install: build
	install -D -m 0755 bin/pe $(DESTDIR)$(BINDIR)/pe
	install -D -m 0755 bin/pe-index $(DESTDIR)$(BINDIR)/pe-index
	install -D -m 0644 man/pe.8 $(DESTDIR)$(MANDIR)/man8/pe.8
	install -D -m 0644 man/pe-index.8 $(DESTDIR)$(MANDIR)/man8/pe-index.8
	install -D -m 0644 completions/bash/pe $(DESTDIR)$(BASHCOMPDIR)/pe
	ln -sf pe $(DESTDIR)$(BASHCOMPDIR)/pe-index
	install -D -m 0644 completions/bash/pe.profile.sh $(DESTDIR)$(PROFILED)/pe-completion.sh
	if [ ! -f $(DESTDIR)$(ETCDIR)/pe.ini ]; then \
		install -D -m 0644 pe.ini.example $(DESTDIR)$(ETCDIR)/pe.ini; \
	fi

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/pe \
	      $(DESTDIR)$(BINDIR)/pe-index \
	      $(DESTDIR)$(MANDIR)/man8/pe.8 \
	      $(DESTDIR)$(MANDIR)/man8/pe-index.8 \
	      $(DESTDIR)$(BASHCOMPDIR)/pe \
	      $(DESTDIR)$(BASHCOMPDIR)/pe-index \
	      $(DESTDIR)$(PROFILED)/pe-completion.sh

clean:
	rm -rf bin
