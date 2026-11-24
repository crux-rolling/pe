package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gokrazy/rsync/rsyncclient"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

const rsyncDialTimeout = 15 * time.Second

type rsyncTarget struct {
	Host       string
	Port       string
	RemotePath string // "<module>" or "<module>/<subpath>/"
}

type nopWriteCloser struct {
	*bytes.Buffer
}

func (nopWriteCloser) Close() error { return nil }

// syncRsync synchronizes a CRUX ports collection using the pure-Go rsync daemon
// protocol implementation in github.com/gokrazy/rsync/rsyncclient (zero os/exec,
// zero external rsync binary required).
//
// Supported URL forms:
//   - rsync://host[:port]/module[/subpath]
//   - host::module[/subpath]
func syncRsync(ctx context.Context, src sources.Source) (SyncOutcome, error) {
	target, err := parseRsyncURL(src.URL)
	if err != nil {
		return SyncOutcome{}, err
	}

	if err := os.MkdirAll(src.DestDir, 0o755); err != nil {
		return SyncOutcome{}, fmt.Errorf("cannot create destination directory %s: %w", src.DestDir, err)
	}

	dialer := &net.Dialer{Timeout: rsyncDialTimeout}
	addr := net.JoinHostPort(target.Host, target.Port)
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return SyncOutcome{}, errors.New("sync canceled")
		}
		return SyncOutcome{}, translateRsyncError(err, addr, src.URL)
	}
	defer conn.Close()

	var errBuf bytes.Buffer
	// DontRestrict disables per-process Landlock sandboxing so concurrent worker
	// goroutines can sync independent collections in the same process.
	client, err := rsyncclient.New(
		[]string{"-a", "--delete"},
		rsyncclient.DontRestrict(),
		rsyncclient.WithStderr(nopWriteCloser{&errBuf}),
	)
	if err != nil {
		return SyncOutcome{}, fmt.Errorf("initialize native rsync client: %w", err)
	}

	res, err := client.RunDaemon(ctx, conn, target.RemotePath, []string{src.DestDir})
	if err != nil {
		if ctx.Err() != nil {
			return SyncOutcome{}, errors.New("sync canceled")
		}
		stderrMsg := strings.TrimSpace(errBuf.String())
		if stderrMsg != "" {
			return SyncOutcome{}, fmt.Errorf("rsync daemon error for %s: %s (%w)", src.URL, stderrMsg, err)
		}
		return SyncOutcome{}, translateRsyncError(err, addr, src.URL)
	}

	if res != nil && res.Stats != nil {
		if res.Stats.Read == 0 && res.Stats.Written == 0 {
			return SyncOutcome{Skipped: true, Detail: "up-to-date"}, nil
		}
		return SyncOutcome{
			Detail: fmt.Sprintf("received %s", formatBytes(res.Stats.Read)),
		}, nil
	}
	return SyncOutcome{Detail: "synced"}, nil
}

func parseRsyncURL(raw string) (rsyncTarget, error) {
	if strings.HasPrefix(raw, "rsync://") {
		u, err := url.Parse(raw)
		if err != nil {
			return rsyncTarget{}, fmt.Errorf("invalid rsync URL %q: %w", raw, err)
		}
		host := u.Hostname()
		if host == "" {
			return rsyncTarget{}, fmt.Errorf("invalid rsync URL %q: missing host", raw)
		}
		port := u.Port()
		if port == "" {
			port = "873"
		}
		remotePath := strings.TrimPrefix(u.Path, "/")
		if remotePath == "" {
			return rsyncTarget{}, fmt.Errorf("invalid rsync URL %q: missing module path", raw)
		}
		if !strings.HasSuffix(remotePath, "/") {
			remotePath += "/"
		}
		return rsyncTarget{Host: host, Port: port, RemotePath: remotePath}, nil
	}

	if strings.Contains(raw, "::") {
		parts := strings.SplitN(raw, "::", 2)
		hostPart := strings.TrimSpace(parts[0])
		modPath := strings.TrimSpace(parts[1])
		if hostPart == "" || modPath == "" {
			return rsyncTarget{}, fmt.Errorf("invalid rsync daemon URL %q: expected host::module/path", raw)
		}
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host := strings.Trim(hostPart, "[]")
		port := "873"
		if h, p, err := net.SplitHostPort(hostPart); err == nil && h != "" && p != "" {
			host, port = h, p
		}
		modPath = strings.TrimPrefix(modPath, "/")
		if !strings.HasSuffix(modPath, "/") {
			modPath += "/"
		}
		return rsyncTarget{Host: host, Port: port, RemotePath: modPath}, nil
	}

	return rsyncTarget{}, fmt.Errorf("pure-Go rsync supports rsync://host[:port]/module/path or host::module/path, got %q", raw)
}

func translateRsyncError(err error, addr, rawURL string) error {
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "no such host"),
		strings.Contains(lower, "temporary failure in name resolution"):
		return fmt.Errorf("DNS resolution failed for %s: check /etc/resolv.conf or network link", rawURL)
	case strings.Contains(lower, "connection refused"):
		return fmt.Errorf("connection refused by rsync daemon %s: verify host and port", addr)
	case strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "deadline exceeded"):
		return fmt.Errorf("rsync connection timed out for %s: check network link or firewall", addr)
	case strings.Contains(lower, "unknown module"):
		return fmt.Errorf("remote rsync daemon rejected module for %s: %v", rawURL, err)
	}
	return fmt.Errorf("rsync sync failed for %s: %w", rawURL, err)
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
