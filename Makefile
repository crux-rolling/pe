PREFIX ?= /usr
BINDIR ?= $(PREFIX)/bin
MANDIR ?= $(PREFIX)/share/man
ETCDIR ?= /etc
DESTDIR ?=

VERSION ?= 0.1.0
LDFLAGS := -s -w -X github.com/ezke5/pe/internal/cli.Version=$(VERSION)

.PHONY: all build test verify clean install

all: build

build: bin/pe bin/pe-index

bin/pe:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/pe ./cmd/pe

bin/pe-index:
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/pe-index ./cmd/pe-index

test:
	go test -v ./...

verify: test build

install: build
	install -D -m 0755 bin/pe $(DESTDIR)$(BINDIR)/pe
	install -D -m 0755 bin/pe-index $(DESTDIR)$(BINDIR)/pe-index
	install -D -m 0644 man/pe.8 $(DESTDIR)$(MANDIR)/man8/pe.8
	install -D -m 0644 man/pe-index.8 $(DESTDIR)$(MANDIR)/man8/pe-index.8
	if [ ! -f $(DESTDIR)$(ETCDIR)/pe.ini ]; then \
		install -D -m 0644 pe.ini.example $(DESTDIR)$(ETCDIR)/pe.ini; \
	fi

clean:
	rm -rf bin
