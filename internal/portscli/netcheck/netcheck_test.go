package netcheck

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

func TestMissingResolvConf(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "resolv.conf")
	sources := []sources.Source{
		{Protocol: sources.ProtoGit, Collection: "core", URL: "https://git.crux.nu/ports/core.git"},
	}

	rep := Run(context.Background(), sources, Options{
		Timeout:        500 * time.Millisecond,
		ResolvConfPath: missing,
	})
	if rep.OK() {
		t.Fatal("expected failure when resolv.conf is missing")
	}
	msg := rep.FirstError()
	if !strings.Contains(msg, "missing") {
		t.Errorf("expected actionable missing resolv.conf message, got: %q", msg)
	}
}

func TestEmptyResolvConf(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(p, []byte("# no nameservers here\nsearch local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := []sources.Source{
		{Protocol: sources.ProtoHTTPUp, Collection: "test", URL: "https://example.org/"},
	}

	rep := Run(context.Background(), sources, Options{
		Timeout:        500 * time.Millisecond,
		ResolvConfPath: p,
	})
	if rep.OK() {
		t.Fatal("expected failure when resolv.conf has no nameservers")
	}
	if !strings.Contains(rep.FirstError(), "nameserver") {
		t.Errorf("expected nameserver hint, got: %q", rep.FirstError())
	}
}

func TestBrokenSymlinkResolvConf(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "resolv.conf")
	if err := os.Symlink(filepath.Join(dir, "nonexistent-target"), link); err != nil {
		t.Fatal(err)
	}

	rep := Run(context.Background(), nil, Options{
		Timeout:           500 * time.Millisecond,
		ResolvConfPath:    link,
		RequireResolvConf: true,
	})
	if rep.OK() {
		t.Fatal("expected failure on broken resolv.conf symlink")
	}
	if !strings.Contains(rep.FirstError(), "broken symlink") {
		t.Errorf("expected broken symlink diagnostic, got: %q", rep.FirstError())
	}
}

func TestReachableAndRefusedLocalTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(resolv, []byte("nameserver 1.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Test reachable endpoint
	sources := []sources.Source{
		{Protocol: sources.ProtoHTTPUp, Collection: "local", URL: "http://" + ln.Addr().String() + "/"},
	}
	rep := Run(context.Background(), sources, Options{
		Timeout:        2 * time.Second,
		ResolvConfPath: resolv,
	})
	if !rep.OK() {
		t.Fatalf("expected reachable local listener, got error: %s", rep.FirstError())
	}

	// Close listener to trigger connection refused and verify clean message
	_ = ln.Close()
	rep2 := Run(context.Background(), sources, Options{
		Timeout:        2 * time.Second,
		ResolvConfPath: resolv,
	})
	if rep2.OK() {
		t.Fatal("expected failure after closing listener")
	}
	msg := rep2.FirstError()
	if !strings.Contains(strings.ToLower(msg), "connection refused") {
		t.Errorf("expected clean connection refused message, got: %q", msg)
	}
	if strings.Contains(msg, "getaddrinfo") || strings.Contains(msg, "socket code") {
		t.Errorf("raw libc/socket error leaked: %q", msg)
	}
}

func TestExtractEndpoints(t *testing.T) {
	sources := []sources.Source{
		{Protocol: sources.ProtoGit, URL: "https://git.crux.nu/ports/core.git"},
		{Protocol: sources.ProtoGit, URL: "https://git.crux.nu/ports/opt.git"}, // duplicate host:port
		{Protocol: sources.ProtoGit, URL: "git@github.com:kryx-linux/kpmcompat.git"},
		{Protocol: sources.ProtoGit, URL: "file:///tmp/local.git"},
		{Protocol: sources.ProtoRsync, URL: "rsync.crux.nu::ports/crux-3.8/core/"},
		{Protocol: sources.ProtoHTTPUp, URL: "http://mirror.local:8080/ports/"},
	}

	eps := ExtractEndpoints(sources)
	if len(eps) != 4 {
		t.Fatalf("expected 4 unique remote endpoints, got %d: %+v", len(eps), eps)
	}

	want := map[string]bool{
		"git.crux.nu:443":   true,
		"github.com:22":     true,
		"rsync.crux.nu:873": true,
		"mirror.local:8080": true,
	}
	for _, ep := range eps {
		if !want[ep.Address()] {
			t.Errorf("unexpected endpoint %s", ep.Address())
		}
	}
}
