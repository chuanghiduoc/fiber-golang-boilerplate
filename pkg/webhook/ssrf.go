package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
)

// Guard validates outbound targets and resolves them to vetted IP addresses.
// It blocks private, loopback, link-local, multicast, unspecified, and
// reserved ranges unless the exact hostname, IP, or CIDR is allowlisted.
//
// Dialing the resolved IP rather than the hostname prevents DNS rebinding
// between validation and connection establishment.
type Guard struct {
	allowedIPs   []*net.IPNet
	allowedHosts map[string]struct{}
	dialTimeout  time.Duration
	resolver     *net.Resolver
}

// NewGuard builds a Guard from operator-supplied allowlist entries. Each entry
// may be an IP, a CIDR, or a hostname (case-insensitive, trailing dot ignored).
// Public targets are always allowed; entries only relax the private/blocked
// check for matching destinations.
func NewGuard(allowlist []string) (*Guard, error) {
	g := &Guard{
		allowedHosts: make(map[string]struct{}, len(allowlist)),
		dialTimeout:  5 * time.Second,
		resolver:     net.DefaultResolver,
	}
	for _, raw := range allowlist {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, network, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("webhook: invalid allowlist CIDR %q: %w", entry, err)
			}
			g.allowedIPs = append(g.allowedIPs, network)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			bits := 128
			if v4 := ip.To4(); v4 != nil {
				ip = v4
				bits = 32
			}
			g.allowedIPs = append(g.allowedIPs, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		host := normalizeHost(entry)
		if host == "" || strings.ContainsAny(host, " \t\r\n/") {
			return nil, fmt.Errorf("webhook: invalid allowlist host %q", entry)
		}
		g.allowedHosts[host] = struct{}{}
	}
	return g, nil
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
	return u, nil
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
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
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
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmark
			return true
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // 198.51.100.0/24 TEST-NET-2
			return true
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // 203.0.113.0/24 TEST-NET-3
			return true
		}
		return false
	}

	if v6 := ip.To16(); v6 != nil {
		// 2001:db8::/32 documentation range.
		if v6[0] == 0x20 && v6[1] == 0x01 && v6[2] == 0x0d && v6[3] == 0xb8 {
			return true
		}
	}
	return false
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
