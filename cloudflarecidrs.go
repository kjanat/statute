package statute

import "statute.kjanat.dev/internal/cloudflare"

// CloudflareCIDRs returns a fresh copy of Cloudflare's published IPv4 and
// IPv6 proxy ranges, suitable for TrustedProxy. It does not select a client-IP
// header or enable BehindCloudflare; configure those independently.
//
// The generated embedded fallback records its fetch timestamp and preserves
// published order. This helper stays offline; use CloudflareTrustedProxy for
// startup and periodic refresh. Callers may extend or replace the static copy.
//
// Run make generate-cloudflare-cidrs before building to refresh the fallback.
// docs/cloudflare.md describes acquisition, scheduling, and failure behavior.
func CloudflareCIDRs() []string {
	return cloudflare.Bundled().CIDRs()
}
