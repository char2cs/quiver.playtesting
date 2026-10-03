package gateway

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Cloudflare edge ranges, copied from https://www.cloudflare.com/ips-v4 and
// https://www.cloudflare.com/ips-v6 on 2026-10-03. Cloudflare changes these rarely;
// override with --trusted-proxies if they do.
var cloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// CloudflareProxies returns Cloudflare's published ranges.
func CloudflareProxies() []netip.Prefix {
	out := make([]netip.Prefix, len(cloudflareRanges))
	for i, s := range cloudflareRanges {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// ParseTrustedProxies parses a comma separated list of CIDRs or IPs. The word
// "cloudflare" (or an empty string) expands to Cloudflare's ranges and "none" trusts nobody.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "none") {
		return nil, nil
	}
	if s == "" {
		return CloudflareProxies(), nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.EqualFold(part, "cloudflare"):
			out = append(out, CloudflareProxies()...)
		case strings.Contains(part, "/"):
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, p.Masked())
		default:
			a, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out, nil
}

func peerAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func trusted(list []netip.Prefix, a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
