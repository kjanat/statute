package statute

// CloudflareCIDRs returns a fresh copy of Cloudflare's published IPv4 and
// IPv6 proxy ranges, suitable for TrustedProxy. It does not select a client-IP
// header or enable BehindCloudflare; configure those independently.
//
// This release-bundled snapshot was checked on 2026-09-28 against
// https://www.cloudflare.com/ips-v4/# and https://www.cloudflare.com/ips-v6/#.
// It preserves their order and does not refresh automatically or access the
// network. Operators may append to or replace it with explicit CIDRs.
//
// Maintainers can check for upstream changes with make check-cloudflare-cidrs;
// docs/cloudflare.md describes the snapshot update procedure.
func CloudflareCIDRs() []string {
	return []string{
		"173.245.48.0/20",
		"103.21.244.0/22",
		"103.22.200.0/22",
		"103.31.4.0/22",
		"141.101.64.0/18",
		"108.162.192.0/18",
		"190.93.240.0/20",
		"188.114.96.0/20",
		"197.234.240.0/22",
		"198.41.128.0/17",
		"162.158.0.0/15",
		"104.16.0.0/13",
		"104.24.0.0/14",
		"172.64.0.0/13",
		"131.0.72.0/22",
		"2400:cb00::/32",
		"2606:4700::/32",
		"2803:f800::/32",
		"2405:b500::/32",
		"2405:8100::/32",
		"2a06:98c0::/29",
		"2c0f:f248::/32",
	}
}
