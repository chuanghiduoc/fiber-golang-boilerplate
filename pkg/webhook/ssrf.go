package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrInvalidTarget is returned when a delivery URL is malformed or uses an
	// unsupported scheme. It is permanent: retrying cannot fix the URL.
	ErrInvalidTarget = errors.New("webhook: invalid target URL")
	// ErrBlockedTarget is returned when the target resolves to a private,
	// loopback, link-local, or otherwise non-public address that is not on the
	// operator allowlist. It is permanent and prevents SSRF.
	ErrBlockedTarget = errors.New("webhook: target address is blocked")
	// ErrPortNotAllowed is returned when a delivery URL (or the dialed
	// address) uses a port outside the outbound policy. It is permanent.
	ErrPortNotAllowed = errors.New("webhook: target port is not allowed")
)

// defaultAllowedPorts is the baseline destination policy: HTTPS and HTTP only.
// Operators may extend it with WEBHOOK_ALLOWED_PORTS.
var defaultAllowedPorts = []int{443, 80}

// Guard validates outbound targets, their port, and resolves them to vetted IP
// addresses. It blocks private, loopback, link-local, multicast, unspecified,
// and reserved ranges unless the exact hostname, IP, or CIDR is allowlisted.
//
// Dialing the resolved IP rather than the hostname prevents DNS rebinding
// between validation and connection establishment.
type Guard struct {
	allowedIPs   []*net.IPNet
	allowedHosts map[string]struct{}
	allowedPorts map[int]struct{}
	dialTimeout  time.Duration
	resolver     *net.Resolver
	lookupIP     func(context.Context, string) ([]net.IPAddr, error)
}

// NewGuard builds a Guard from operator-supplied allowlist entries with the
// default outbound ports (443 and 80). Each entry may be an IP, a CIDR, or a
// hostname (case-insensitive, trailing dot ignored). Public targets are always
// allowed; entries only relax the private/blocked check for matching
// destinations.
//
// The dial timeout defaults to 5s; a Client replaces it with its configured
// ConnectTimeout.
func NewGuard(allowlist []string) (*Guard, error) {
	return NewGuardWithPorts(allowlist, nil)
}

// NewGuardWithPorts builds a Guard like NewGuard and extends the allowed
// destination ports with ports. Entries must be 1-65535; 443 and 80 always
// remain allowed.
func NewGuardWithPorts(allowlist []string, ports []int) (*Guard, error) {
	g := &Guard{
		allowedHosts: make(map[string]struct{}, len(allowlist)),
		allowedPorts: make(map[int]struct{}, len(ports)+len(defaultAllowedPorts)),
		dialTimeout:  defaultConnectTimeout,
		resolver:     net.DefaultResolver,
	}
	g.lookupIP = g.resolver.LookupIPAddr
	for _, port := range defaultAllowedPorts {
		g.allowedPorts[port] = struct{}{}
	}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("%w: port %d must be between 1 and 65535", ErrPortNotAllowed, port)
		}
		g.allowedPorts[port] = struct{}{}
	}
	for _, raw := range allowlist {
		if err := g.allow(strings.TrimSpace(raw)); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// allow adds one allowlist entry: a CIDR, a single IP (stored as a host
// network), or a hostname. Blank entries are ignored.
func (g *Guard) allow(entry string) error {
	if entry == "" {
		return nil
	}
	if strings.Contains(entry, "/") {
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return fmt.Errorf("webhook: invalid allowlist CIDR %q: %w", entry, err)
		}
		g.allowedIPs = append(g.allowedIPs, network)
		return nil
	}
	if ip := net.ParseIP(entry); ip != nil {
		bits := 128
		if v4 := ip.To4(); v4 != nil {
			ip = v4
			bits = 32
		}
		g.allowedIPs = append(g.allowedIPs, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		return nil
	}
	host := normalizeHost(entry)
	if host == "" || strings.ContainsAny(host, " \t\r\n/") {
		return fmt.Errorf("webhook: invalid allowlist host %q", entry)
	}
	g.allowedHosts[host] = struct{}{}
	return nil
}

// WithLookupIP returns a copy of g whose DNS resolution uses lookup. It exists
// for validation-time resolution in tests and for deployments with a custom
// resolver; the original Guard is unchanged.
func (g *Guard) WithLookupIP(lookup func(context.Context, string) ([]net.IPAddr, error)) *Guard {
	clone := *g
	if lookup != nil {
		clone.lookupIP = lookup
	}
	return &clone
}

// withDialTimeout returns a copy of g whose dialer uses timeout for the TCP
// connect. The allowlist is shared read-only, so the copy is cheap and the
// original Guard keeps its own timeout.
func (g *Guard) withDialTimeout(timeout time.Duration) *Guard {
	clone := *g
	clone.dialTimeout = timeout
	return &clone
}

// ValidateURL parses rawURL and enforces the delivery URL contract: absolute
// http(s), a host, no userinfo. It does not resolve DNS; use CheckHost or a
// DialContext guarded Client for that.
func (g *Guard) ValidateURL(rawURL string) (*url.URL, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("%w: url is empty", ErrInvalidTarget)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme must be http or https", ErrInvalidTarget)
	}
	if u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: host is required", ErrInvalidTarget)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo is not allowed", ErrInvalidTarget)
	}
	port, err := urlPort(u)
	if err != nil {
		return nil, err
	}
	if !g.portAllowed(port) {
		return nil, fmt.Errorf("%w: port %d is not allowed", ErrPortNotAllowed, port)
	}
	return u, nil
}

// urlPort returns the explicit port of u, or the scheme default when the URL
// omits one. A bare trailing colon is treated as malformed.
func urlPort(u *url.URL) (int, error) {
	raw := u.Port()
	if raw == "" {
		if strings.HasSuffix(u.Host, ":") {
			return 0, fmt.Errorf("%w: port is empty", ErrInvalidTarget)
		}
		switch u.Scheme {
		case "http":
			return 80, nil
		case "https":
			return 443, nil
		default:
			return 0, fmt.Errorf("%w: scheme must be http or https", ErrInvalidTarget)
		}
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%w: invalid port %q", ErrInvalidTarget, raw)
	}
	return port, nil
}

// portAllowed reports whether port is in the outbound destination policy.
func (g *Guard) portAllowed(port int) bool {
	_, ok := g.allowedPorts[port]
	return ok
}

// CheckIP reports whether ip may be dialed. Allowlisted IPs/CIDRs bypass the
// blocked-range check; every other blocked address is rejected.
func (g *Guard) CheckIP(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("%w: empty address", ErrBlockedTarget)
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if g.ipAllowed(ip) {
		return nil
	}
	if isBlockedIP(ip) {
		return fmt.Errorf("%w: %s is not a public address", ErrBlockedTarget, ip)
	}
	return nil
}

// CheckHost resolves host and returns the addresses that may be dialed. A
// hostname or IP on the allowlist is accepted even when it is private; for
// unlisted hosts, public addresses are returned and blocked addresses are
// omitted. When no address remains, ErrBlockedTarget is returned so callers
// cannot fall back to the original hostname.
func (g *Guard) CheckHost(ctx context.Context, host string) ([]net.IP, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrInvalidTarget)
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := g.CheckIP(ip); err != nil {
			return nil, err
		}
		return []net.IP{ip}, nil
	}

	hostAllowed := g.hostAllowed(host)
	lookup := g.lookupIP
	if lookup == nil {
		lookup = g.resolver.LookupIPAddr
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("webhook: resolve %q: %w", host, err)
	}
	allowed := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		ip := addr.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if !hostAllowed && isBlockedIP(ip) && !g.ipAllowed(ip) {
			continue
		}
		allowed = append(allowed, ip)
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%w: %q resolved only to blocked addresses", ErrBlockedTarget, host)
	}
	return allowed, nil
}

// DialContext resolves and validates the target, then dials one of the vetted
// IPs. The URL hostname remains unchanged for HTTP Host and TLS SNI.
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}
	// Defense in depth: Client.Do validates the URL, but the dialer enforces
	// the port policy again so no caller can bypass it by constructing an
	// address directly.
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, fmt.Errorf("%w: invalid port %q", ErrInvalidTarget, port)
	}
	if !g.portAllowed(portNumber) {
		return nil, fmt.Errorf("%w: port %d is not allowed", ErrPortNotAllowed, portNumber)
	}
	ips, err := g.CheckHost(ctx, host)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: g.dialTimeout, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = ErrBlockedTarget
	}
	return nil, fmt.Errorf("webhook: dial %q: %w", host, lastErr)
}

func (g *Guard) ipAllowed(ip net.IP) bool {
	for _, network := range g.allowedIPs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (g *Guard) hostAllowed(host string) bool {
	_, ok := g.allowedHosts[host]
	return ok
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// isBlockedIP classifies an address using the standard library plus the
// reserved IPv4/IPv6 ranges that Go does not classify as private.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return isReservedIPv4(v4)
	}
	if v6 := ip.To16(); v6 != nil {
		return isBlockedIPv6(v6)
	}
	return false
}

// isReservedIPv4 covers the special-purpose IPv4 ranges the standard library
// does not classify as private.
func isReservedIPv4(v4 net.IP) bool {
	switch {
	case v4[0] == 0: // 0.0.0.0/8 "this network"
		return true
	case v4[0] >= 240: // 240.0.0.0/4 reserved + 255.255.255.255
		return true
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
		return true
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 2: // 192.0.2.0/24 TEST-NET-1
		return true
	case v4[0] == 192 && v4[1] == 88 && v4[2] == 99: // 192.88.99.0/24 6to4 relay anycast
		return true
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmark
		return true
	case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // 198.51.100.0/24 TEST-NET-2
		return true
	case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // 203.0.113.0/24 TEST-NET-3
		return true
	}
	return false
}

// blockedIPv6Ranges are non-public IPv6 ranges with no usable embedded IPv4
// destination. Teredo is blocked whole: its client address is obfuscated and
// the relay path is not something a webhook target legitimately needs.
var blockedIPv6Ranges = mustCIDRs(
	"2001:db8::/32",  // documentation
	"2001::/32",      // Teredo
	"2001:2::/48",    // benchmarking
	"fec0::/10",      // deprecated site-local
	"100::/64",       // discard-only
	"64:ff9b:1::/48", // local-use NAT64 (RFC 8215)
)

// embeddedIPv4Ranges translate or tunnel to the IPv4 address at the given
// byte offset, so the target is only as public as that IPv4 address. A dial
// to e.g. 64:ff9b::7f00:1 through a NAT64 gateway reaches 127.0.0.1.
var embeddedIPv4Ranges = []struct {
	network *net.IPNet
	offset  int
}{
	{mustCIDRs("64:ff9b::/96")[0], 12},    // well-known NAT64 (RFC 6052)
	{mustCIDRs("2002::/16")[0], 2},        // 6to4 (RFC 3056)
	{mustCIDRs("::/96")[0], 12},           // deprecated IPv4-compatible
	{mustCIDRs("::ffff:0:0:0/96")[0], 12}, // SIIT IPv4-translated (RFC 2765)
}

func isBlockedIPv6(v6 net.IP) bool {
	for _, network := range blockedIPv6Ranges {
		if network.Contains(v6) {
			return true
		}
	}
	for _, r := range embeddedIPv4Ranges {
		if r.network.Contains(v6) {
			return isBlockedIP(net.IP(v6[r.offset : r.offset+net.IPv4len]))
		}
	}
	return false
}

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("webhook: invalid built-in CIDR %q: %v", cidr, err))
		}
		out = append(out, network)
	}
	return out
}

// IsBlocked reports whether err (or an error it wraps) is a blocked-target
// error. Delivery workers treat blocked targets as permanent failures.
func IsBlocked(err error) bool {
	return errors.Is(err, ErrBlockedTarget)
}

// IsInvalidTarget reports whether err is a permanently invalid target URL.
func IsInvalidTarget(err error) bool {
	return errors.Is(err, ErrInvalidTarget)
}

// IsPortNotAllowed reports whether err is a permanently disallowed target
// port.
func IsPortNotAllowed(err error) bool {
	return errors.Is(err, ErrPortNotAllowed)
}
