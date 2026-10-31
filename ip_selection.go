package statute

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"statute.kjanat.dev/resolved"
)

// IPSelection chooses a verified client, direct peer or forwarded-chain address.
// Forwarded mode uses Depth from the right, or skips ExcludedIPs from the right.
type IPSelection = resolved.IPSelection

func resolveIPSelection(p *IPSelection) (*resolved.IPSelection, error) {
	if p == nil {
		return nil, nil
	}
	out := *p
	if !slices.Contains([]string{"peer", "verified", "forwarded"}, out.Mode) {
		return nil, errors.New("ip_selection: invalid mode")
	}
	if out.Depth < 0 {
		return nil, errors.New("ip_selection: depth must be nonnegative")
	}
	var excluded []string
	for _, value := range out.ExcludedIPs {
		if addr, err := netip.ParseAddr(value); err == nil {
			value = netip.PrefixFrom(addr, addr.BitLen()).String()
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, errors.New("ip_selection: invalid excluded IP or CIDR")
		}
		excluded = append(excluded, prefix.Masked().String())
	}
	out.ExcludedIPs = excluded
	if out.IPv6Subnet != nil {
		bits := *out.IPv6Subnet
		if bits < 0 || bits > 128 {
			return nil, errors.New("ip_selection: IPv6 subnet must be between 0 and 128")
		}
		out.IPv6Subnet = &bits
	}
	return &out, nil
}

func selectedRequestIP(p *resolved.IPSelection, r *http.Request) string {
	if p == nil {
		return clientIP(r)
	}
	value := clientIP(r)
	if p.Mode != "verified" {
		value = r.RemoteAddr
		if host, _, err := net.SplitHostPort(value); err == nil {
			value = host
		}
		if p.Mode == "forwarded" {
			value = forwardedSelection(p, r)
		}
	}
	if p.IPv6Subnet != nil {
		if ip, err := netip.ParseAddr(value); err == nil && ip.Is6() {
			if prefix, err := ip.Prefix(*p.IPv6Subnet); err == nil {
				value = prefix.Addr().String()
			}
		}
	}
	return value
}

// SelectIP chooses the address used by this allow list.
func (m *allowIPsMW) SelectIP(policy IPSelection) *allowIPsMW {
	policy.ExcludedIPs = slices.Clone(policy.ExcludedIPs)
	m.selection = &policy
	return m
}

// SelectIP chooses the address used for client-IP rate-limit keys.
func (r *rateLimitMW) SelectIP(policy IPSelection) *rateLimitMW {
	policy.ExcludedIPs = slices.Clone(policy.ExcludedIPs)
	r.selection = &policy
	return r
}

func forwardedSelection(p *resolved.IPSelection, r *http.Request) string {
	value := ""

	parts := strings.Split(r.Header.Get(headerXForwardedFor), ",")
	value = ""
	if p.Depth > 0 {
		if p.Depth <= len(parts) {
			value = strings.TrimSpace(parts[len(parts)-p.Depth])
		}
	} else {
		excluded := mustParsePrefixes(p.ExcludedIPs)
		for _, part := range slices.Backward(parts) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			ip, err := netip.ParseAddr(part)
			if err != nil || !addrInPrefixes(ip, excluded) {
				value = part
				break
			}
		}
	}
	return value
}
