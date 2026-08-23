package statute

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/internal/cloudflare"
	"statute.kjanat.dev/resolved"
)

// newDNS01Manager builds an acmeManager that satisfies DNS-01 challenges
// via Cloudflare's DNS API. Its storage lives under <storage>/dns01.
func newDNS01Manager(cfg *resolved.AutoTLS) (*acmeManager, error) {
	if cfg == nil || cfg.DNS01 == nil {
		return nil, errors.New("dns01: nil config")
	}
	return newACMEManager(cfg, "dns01", &dns01Solver{
		cf:     cloudflare.New(cfg.DNS01.APIToken),
		zoneID: cfg.DNS01.ZoneID,
	})
}

// dns01Solver satisfies ACME DNS-01 challenges by publishing the challenge
// TXT record through Cloudflare's DNS API.
type dns01Solver struct {
	cf     *cloudflare.Client
	zoneID string // optional; empty means auto-discover per host
}

func (*dns01Solver) challengeType() string { return "dns-01" }

func (s *dns01Solver) satisfy(ctx context.Context, client *acme.Client, host, authzURL string, ch *acme.Challenge) error {
	value, err := client.DNS01ChallengeRecord(ch.Token)
	if err != nil {
		return err
	}
	zoneID := s.zoneID
	if zoneID == "" {
		zoneID, err = s.cf.FindZoneID(ctx, host)
		if err != nil {
			return err
		}
	}
	recordName := "_acme-challenge." + strings.TrimPrefix(host, "*.")
	recordID, err := s.cf.AddTXTRecord(ctx, zoneID, recordName, value)
	if err != nil {
		return fmt.Errorf("add TXT record: %w", err)
	}
	defer func() { //nolint:contextcheck // detached ctx on purpose: best-effort cleanup must run even after the parent ctx is cancelled
		// Cleanup is best-effort. Cloudflare's free-tier 60s TTL means a
		// stale record is harmless after a couple of minutes.
		dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if derr := s.cf.DeleteRecord(dctx, zoneID, recordID); derr != nil {
			log.Printf("statute: dns01: cleanup TXT %s: %v", recordName, derr)
		}
	}()

	// DNS propagation: wait briefly before telling ACME to validate.
	// Cloudflare's authoritative resolvers are fast (~10s); we sleep up to
	// 30s and let ACME's own retries handle the rest.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
	}

	if _, err := client.Accept(ctx, ch); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	// Poll the authorization, not the challenge: only the authorization
	// reports its own terminal states (RFC 8555 §7.1.6), and an
	// acme.AuthorizationError built from it names the identifier.
	if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
		return fmt.Errorf("wait authorization: %w", err)
	}
	return nil
}
