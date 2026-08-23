package statute

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"statute.kjanat.dev/internal/cloudflare"
	"statute.kjanat.dev/resolved"
)

// dns01PropagationDelay is how long a DNS-01 source waits between
// publishing the challenge TXT record and asking the CA to validate it
// when no Propagation policy is declared. Cloudflare's authoritative
// resolvers converge in around ten seconds, so the fixed wait is
// conservative and the CA's own retries cover the rest. Deployments whose
// DNS is slower, or that want the record verified rather than assumed,
// declare a statute.DNSPropagation policy instead.
const dns01PropagationDelay = 15 * time.Second

// newDNS01Manager builds an acmeManager that satisfies DNS-01 challenges
// via Cloudflare's DNS API. Its storage lives under <storage>/dns01.
func newDNS01Manager(cfg *resolved.AutoTLS) (*acmeManager, error) {
	if cfg == nil || cfg.DNS01 == nil {
		return nil, errors.New("dns01: nil config")
	}
	return newACMEManager(cfg, "dns01", &dns01Solver{
		cf:     cloudflare.New(cfg.DNS01.APIToken),
		zoneID: cfg.DNS01.ZoneID,
		prop:   cfg.DNS01.Propagation,
	})
}

// dns01Solver satisfies ACME DNS-01 challenges by publishing the challenge
// TXT record through Cloudflare's DNS API.
type dns01Solver struct {
	cf     *cloudflare.Client
	zoneID string // optional; empty means auto-discover per host
	// prop is the source's propagation policy; nil means the fixed
	// dns01PropagationDelay wait.
	prop *resolved.DNSPropagation
	// lookupTXT queries one resolver for a name's TXT records. Nil uses
	// lookupTXTAt, which dials the resolver directly; tests substitute it.
	lookupTXT func(ctx context.Context, resolver, name string) ([]string, error)
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

	return s.validate(ctx, client, authzURL, ch, recordName, value)
}

// validate waits for the published record to propagate, then drives the
// challenge to a validated authorization. The wait comes first and fails
// closed: asking the CA to validate a record it cannot see yet spends one
// of the five validation failures Let's Encrypt allows per hostname per
// hour, while giving up here costs nothing.
func (s *dns01Solver) validate(ctx context.Context, client *acme.Client, authzURL string, ch *acme.Challenge, recordName, value string) error {
	if err := s.awaitPropagation(ctx, recordName, value); err != nil {
		return err
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

// awaitPropagation blocks until the challenge record is believed visible
// to the CA. Without a policy that is the fixed dns01PropagationDelay
// sleep. With one it is the declared delay, followed — when the policy
// names resolvers — by polling those resolvers until every one of them
// serves the expected value.
func (s *dns01Solver) awaitPropagation(ctx context.Context, recordName, value string) error {
	if s.prop == nil {
		return sleepContext(ctx, dns01PropagationDelay)
	}
	if s.prop.Delay > 0 {
		if err := sleepContext(ctx, s.prop.Delay); err != nil {
			return err
		}
	}
	if len(s.prop.Resolvers) == 0 {
		return nil
	}
	return s.pollResolvers(ctx, recordName, value)
}

// sleepContext waits for d, or returns the context's error if it is
// cancelled first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pollResolvers queries every configured resolver for recordName until all
// of them serve value, or the policy's timeout elapses. The first round
// runs immediately — the delay has already been served — and a resolver
// that has answered correctly once is never queried again, so a slow
// resolver cannot un-satisfy itself on a later round.
//
// Lookup errors are not fatal. A record that has not propagated yet looks
// exactly like NXDOMAIN or SERVFAIL from a resolver still serving a stale
// negative answer, so an error only leaves that resolver unsatisfied for
// this round.
func (s *dns01Solver) pollResolvers(ctx context.Context, recordName, value string) error {
	if s.prop.Interval <= 0 {
		// Resolve always fills Interval alongside Resolvers; a zero here
		// means a hand-built policy, and NewTicker would panic on it.
		return fmt.Errorf("dns01: propagation policy names resolvers but no interval")
	}
	pctx, cancel := context.WithTimeout(ctx, s.prop.Timeout)
	defer cancel()

	pending := slices.Clone(s.prop.Resolvers)
	ticker := time.NewTicker(s.prop.Interval)
	defer ticker.Stop()
	for {
		pending = s.stillPending(pctx, pending, recordName, value)
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-pctx.Done():
			// A cancelled parent is the caller giving up, not a
			// propagation failure; report it as itself.
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("dns01: TXT %s not visible to %s after %s", recordName, strings.Join(pending, ", "), s.prop.Timeout)
		case <-ticker.C:
		}
	}
}

// stillPending probes every resolver that has not yet served value and
// returns those that still have not, in their original order. The probes
// run concurrently, each on its own Interval-bounded context. Concurrency
// is the invariant here, not an optimisation: probed one after another
// out of the round's shared budget, a black-holed resolver would spend
// everyone's window and the resolvers listed after it would only ever be
// handed an expired context — which resolver answers would depend on
// declaration order.
func (s *dns01Solver) stillPending(ctx context.Context, pending []string, recordName, value string) []string {
	served := make([]bool, len(pending))
	var wg sync.WaitGroup
	for i, r := range pending {
		wg.Go(func() {
			lctx, cancel := context.WithTimeout(ctx, s.prop.Interval)
			defer cancel()
			served[i] = s.resolverServes(lctx, r, recordName, value)
		})
	}
	wg.Wait()
	remaining := make([]string, 0, len(pending))
	for i, r := range pending {
		if !served[i] {
			remaining = append(remaining, r)
		}
	}
	return remaining
}

// resolverServes reports whether one resolver returns value among the TXT
// records for recordName.
func (s *dns01Solver) resolverServes(ctx context.Context, resolver, recordName, value string) bool {
	lookup := s.lookupTXT
	if lookup == nil {
		lookup = lookupTXTAt
	}
	txt, err := lookup(ctx, resolver, recordName)
	if err != nil {
		return false
	}
	return slices.Contains(txt, value)
}

// lookupTXTAt resolves name's TXT records at one specific DNS server. The
// Go resolver is used directly (PreferGo) with a dialer that keeps the
// requested network — UDP first, TCP on truncation — and only swaps the
// destination address for the configured resolver, so the query bypasses
// the host's own resolver configuration entirely.
func lookupTXTAt(ctx context.Context, resolver, name string) ([]string, error) {
	// Query the rooted name: unrooted, the Go resolver applies the host's
	// search/ndots configuration first — extra round trips per probe, and
	// the host's internal search domains leak to the configured resolver.
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, resolver)
		},
	}
	return r.LookupTXT(ctx, name)
}
