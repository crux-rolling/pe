// Package netcheck provides pre-flight /etc/resolv.conf, DNS resolution, and
// TCP reachability diagnostics with strict timeouts and human-readable errors.
package netcheck

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kryx-linux/kpm/internal/portscli/sources"
)

const (
	// DefaultTimeout is the strict upper bound for all pre-flight checks.
	DefaultTimeout = 4 * time.Second
	// DefaultResolvConf is the standard resolver configuration path on Linux.
	DefaultResolvConf = "/etc/resolv.conf"
)

// Options configures pre-flight network and DNS checks.
type Options struct {
	Timeout        time.Duration
	ResolvConfPath string
	// RequireResolvConf enforces /etc/resolv.conf verification even when no
	// remote endpoints are configured (used by `ports check`).
	RequireResolvConf bool
	// Resolver allows injecting a custom resolver for testing.
	Resolver *net.Resolver
}

// Endpoint identifies a unique remote host and TCP port derived from sources.
type Endpoint struct {
	Host string
	Port string
}

func (e Endpoint) Address() string {
	return net.JoinHostPort(e.Host, e.Port)
}

// Item describes the outcome of a single pre-flight diagnostic step.
type Item struct {
	Label   string
	OK      bool
	Message string
	Elapsed time.Duration
}

// Report aggregates all pre-flight diagnostic outcomes.
type Report struct {
	Items []Item
}

// OK reports whether every diagnostic item succeeded.
func (r *Report) OK() bool {
	for _, it := range r.Items {
		if !it.OK {
			return false
		}
	}
	return true
}

// FirstError returns the first failure message, if any.
func (r *Report) FirstError() string {
	for _, it := range r.Items {
		if !it.OK {
			return it.Message
		}
	}
	return ""
}

// Run executes pre-flight checks for the provided port sources within a strict
// timeout (default 4 seconds). It verifies /etc/resolv.conf, DNS resolution,
// and TCP connectivity for every unique remote endpoint.
func Run(ctx context.Context, sources []sources.Source, opts Options) *Report {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	resolvPath := opts.ResolvConfPath
	if resolvPath == "" {
		resolvPath = DefaultResolvConf
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	endpoints := ExtractEndpoints(sources)
	report := &Report{}

	// If all configured sources are local (e.g. file://) and resolv.conf is not
	// explicitly required, skip remote network checks.
	if len(endpoints) == 0 && !opts.RequireResolvConf {
		report.Items = append(report.Items, Item{
			Label:   "network",
			OK:      true,
			Message: "local-only sources configured; skipping remote network checks",
		})
		return report
	}

	// Step 1: Verify /etc/resolv.conf unless every endpoint is a literal IP
	// (still check when RequireResolvConf is set).
	needDNS := opts.RequireResolvConf || hasHostnames(endpoints)
	if needDNS {
		start := time.Now()
		nsCount, errMsg := checkResolvConf(resolvPath)
		if errMsg != "" {
			report.Items = append(report.Items, Item{
				Label:   "resolv.conf",
				OK:      false,
				Message: errMsg,
				Elapsed: time.Since(start),
			})
			// Without working DNS config, hostname resolution will fail; stop early
			// with the actionable root cause instead of cascading DNS errors.
			if hasHostnames(endpoints) {
				return report
			}
		} else {
			report.Items = append(report.Items, Item{
				Label:   "resolv.conf",
				OK:      true,
				Message: fmt.Sprintf("%s OK (%d nameserver(s))", resolvPath, nsCount),
				Elapsed: time.Since(start),
			})
		}
	}

	if len(endpoints) == 0 {
		return report
	}

	// Step 2 & 3: Check DNS resolution and TCP reachability concurrently within
	// the strict shared timeout budget.
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	results := make([]Item, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func(idx int, target Endpoint) {
			defer wg.Done()
			results[idx] = checkEndpoint(checkCtx, resolver, target, timeout)
		}(i, ep)
	}
	wg.Wait()

	report.Items = append(report.Items, results...)
	return report
}

// CheckResolvConf verifies that path exists, is readable, and defines at least
// one nameserver. Exported for focused testing and diagnostics.
func checkResolvConf(path string) (int, string) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Check whether it is a dangling symlink (common in minimal chroots).
			if linfo, lerr := os.Lstat(path); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
				return 0, fmt.Sprintf("%s is a broken symlink: copy host /etc/resolv.conf into chroot or configure DNS", path)
			}
			return 0, fmt.Sprintf("%s is missing: copy host /etc/resolv.conf into chroot or configure DNS", path)
		}
		if errors.Is(err, os.ErrPermission) {
			return 0, fmt.Sprintf("%s is not readable: check file permissions (expected 0644)", path)
		}
		return 0, fmt.Sprintf("cannot access %s: check filesystem mount or permissions", path)
	}
	if info.IsDir() {
		return 0, fmt.Sprintf("%s is a directory, expected a regular resolver config file", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Sprintf("cannot open %s: check file permissions", path)
	}
	defer f.Close()

	nameservers := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "nameserver") {
			if net.ParseIP(fields[1]) != nil || strings.Contains(fields[1], "%") {
				nameservers++
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Sprintf("failed reading %s: check filesystem health", path)
	}
	if nameservers == 0 {
		return 0, fmt.Sprintf("%s has no valid nameserver entries: add e.g. 'nameserver 1.1.1.1'", path)
	}
	return nameservers, ""
}

func checkEndpoint(ctx context.Context, resolver *net.Resolver, ep Endpoint, timeout time.Duration) Item {
	start := time.Now()
	dialHost := ep.Host

	if net.ParseIP(ep.Host) == nil {
		addrs, err := resolver.LookupHost(ctx, ep.Host)
		if err != nil {
			return Item{
				Label:   ep.Address(),
				OK:      false,
				Message: formatDNSError(ep.Host, err, timeout),
				Elapsed: time.Since(start),
			}
		}
		if len(addrs) == 0 {
			return Item{
				Label:   ep.Address(),
				OK:      false,
				Message: fmt.Sprintf("DNS resolution failed for %s: no addresses returned (check /etc/resolv.conf or network link)", ep.Host),
				Elapsed: time.Since(start),
			}
		}
		// Prefer connecting via the resolved address so DialContext does not
		// perform a redundant DNS lookup.
		dialHost = addrs[0]
	}

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(dialHost, ep.Port))
	if err != nil {
		return Item{
			Label:   ep.Address(),
			OK:      false,
			Message: formatDialError(ep, err, timeout),
			Elapsed: time.Since(start),
		}
	}
	_ = conn.Close()

	return Item{
		Label:   ep.Address(),
		OK:      true,
		Message: fmt.Sprintf("reachable (%s)", ep.Address()),
		Elapsed: time.Since(start),
	}
}

// ExtractEndpoints returns a sorted, deduplicated list of remote TCP endpoints
// required to sync the given sources. Local file:// sources are omitted.
func ExtractEndpoints(sources []sources.Source) []Endpoint {
	seen := make(map[Endpoint]struct{})
	var list []Endpoint

	for _, s := range sources {
		ep, ok := endpointForSource(s)
		if !ok {
			continue
		}
		if _, dup := seen[ep]; !dup {
			seen[ep] = struct{}{}
			list = append(list, ep)
		}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Host != list[j].Host {
			return list[i].Host < list[j].Host
		}
		return list[i].Port < list[j].Port
	})
	return list
}

func endpointForSource(s sources.Source) (Endpoint, bool) {
	raw := s.URL

	// Handle rsync daemon syntax: host::module/path
	if s.Protocol == sources.ProtoRsync && !strings.Contains(raw, "://") && strings.Contains(raw, "::") {
		hostPart := strings.SplitN(raw, "::", 2)[0]
		if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		host := strings.Trim(hostPart, "[]")
		if host == "" {
			return Endpoint{}, false
		}
		return Endpoint{Host: host, Port: "873"}, true
	}

	// Handle SCP-style git/rsync URLs: user@host:path
	if !strings.Contains(raw, "://") {
		if colon := strings.IndexByte(raw, ':'); colon > 0 {
			hostPart := raw[:colon]
			if at := strings.LastIndexByte(hostPart, '@'); at >= 0 {
				hostPart = hostPart[at+1:]
			}
			host := strings.Trim(hostPart, "[]")
			if host == "" {
				return Endpoint{}, false
			}
			return Endpoint{Host: host, Port: "22"}, true
		}
		return Endpoint{}, false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, false
	}
	if u.Scheme == "file" || u.Hostname() == "" {
		return Endpoint{}, false
	}

	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		case "git":
			port = "9418"
		case "ssh":
			port = "22"
		case "rsync":
			port = "873"
		default:
			port = "443"
		}
	}

	return Endpoint{Host: u.Hostname(), Port: port}, true
}

func hasHostnames(endpoints []Endpoint) bool {
	for _, ep := range endpoints {
		if net.ParseIP(ep.Host) == nil {
			return true
		}
	}
	return false
}

func formatDNSError(host string, err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("DNS resolution timed out for %s (%s): check /etc/resolv.conf or network link", host, timeout)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout {
			return fmt.Sprintf("DNS resolution timed out for %s (%s): check /etc/resolv.conf or network link", host, timeout)
		}
		if dnsErr.IsNotFound {
			return fmt.Sprintf("DNS resolution failed for %s: host not found (check URL and /etc/resolv.conf)", host)
		}
	}
	if isNetworkDown(err) {
		return fmt.Sprintf("DNS resolution failed for %s: network interface is offline or no default route", host)
	}
	return fmt.Sprintf("DNS resolution failed for %s: check /etc/resolv.conf or network link", host)
}

func formatDialError(ep Endpoint, err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("TCP connection to %s timed out (%s): check firewall, proxy, or network link", ep.Address(), timeout)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Sprintf("TCP connection to %s timed out (%s): check firewall, proxy, or network link", ep.Address(), timeout)
	}
	if isNetworkDown(err) {
		return fmt.Sprintf("Network unreachable for %s: check network interface link and default route", ep.Address())
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Sprintf("Connection refused by %s: verify remote host, port, and URL", ep.Address())
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return fmt.Sprintf("Host unreachable (%s): check routing table or remote host status", ep.Address())
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return fmt.Sprintf("Connection reset by %s: remote service closed the connection", ep.Address())
	}
	return fmt.Sprintf("Unable to reach %s: check network link and firewall rules", ep.Address())
}

func isNetworkDown(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.ENETDOWN)
}
