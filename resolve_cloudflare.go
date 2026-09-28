package statute

import (
	"errors"
	"fmt"
	"slices"

	"statute.kjanat.dev/internal/cloudflare"
	"statute.kjanat.dev/resolved"
)

// resolveCloudflareFallback keeps artifact validation on the offline boundary.
func resolveCloudflareFallback(policy *TrustedProxyConfig, listener *resolved.Listener) error {
	if !policy.fallbackSet {
		return nil
	}
	if !policy.cloudflare {
		return errors.New("trusted_proxy: FallbackSnapshot requires CloudflareTrustedProxy")
	}
	snapshot, err := cloudflare.DecodeSnapshot(policy.fallbackSnapshot)
	if err != nil {
		return fmt.Errorf("cloudflare fallback: %w", err)
	}
	listener.CloudflareFallback = &resolved.CloudflareSnapshot{
		IPv4: snapshot.IPv4, IPv6: snapshot.IPv6, FetchedAt: snapshot.FetchedAt,
	}
	return nil
}

// listenerCloudflareFallback creates an independent normalized effective value.
func listenerCloudflareFallback(listener *resolved.Listener) cloudflare.Snapshot {
	if snapshot := listener.CloudflareFallback; snapshot != nil {
		return (cloudflare.Snapshot{IPv4: snapshot.IPv4, IPv6: snapshot.IPv6, FetchedAt: snapshot.FetchedAt}).Normalized()
	}
	return cloudflare.Bundled().Normalized()
}

// validateCloudflareFallbacks protects the one server-owned range source from
// silently choosing one listener's fallback policy or provenance for another.
func validateCloudflareFallbacks(listeners []*resolved.Listener) error {
	var first *cloudflare.Snapshot
	firstIndex := 0
	for i, listener := range listeners {
		if !listener.CloudflareTrustedProxy {
			continue
		}
		snapshot := listenerCloudflareFallback(listener)
		if first == nil {
			first, firstIndex = &snapshot, i
			continue
		}
		if !sameCloudflareFallback(*first, snapshot) {
			return fmt.Errorf("listener[%d]: cloudflare fallback conflicts with listener[%d]; managed listeners must share ranges and fetched_at", i, firstIndex)
		}
	}
	return nil
}

// sameCloudflareFallback compares normalized sets and the source time instant.
func sameCloudflareFallback(a, b cloudflare.Snapshot) bool {
	return slices.Equal(a.IPv4, b.IPv4) && slices.Equal(a.IPv6, b.IPv6) && a.FetchedAt.Equal(b.FetchedAt)
}
