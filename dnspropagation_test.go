package statute

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/net/dns/dnsmessage"

	"statute.kjanat.dev/resolved"
)

// propagationSource builds the issue's DNS-01 chain, optionally carrying a
// propagation policy, and resolves it.
func propagationSource(p *DNSPropagation) Config {
	src := AutoTLS("*.foo.example.test").
		Email("ops@example.test").
		Storage("/var/lib/statute/acme").
		CloudflareDNS01("cf-token")
	if p != nil {
		src = src.Propagation(*p)
	}
	return tlsRouterConfig(src)
}

// resolvePropagation resolves a DNS-01 source carrying p and returns the
// normalised policy.
func resolvePropagation(t *testing.T, p *DNSPropagation) *resolved.DNSPropagation {
	t.Helper()
	r, err := Resolve(propagationSource(p))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return r.Listeners[0].AutoTLSSources[0].DNS01.Propagation
}

// TestResolveDNSPropagation_IssueExample pins the exact shape issue #39
// asks for: a delay plus two resolvers, with the polling window filled
// from defaults and the resolvers kept in declaration order.
func TestResolveDNSPropagation_IssueExample(t *testing.T) {
	t.Parallel()
	got := resolvePropagation(t, &DNSPropagation{
		Delay:     "30s",
		Resolvers: []string{"192.0.2.53:53", "198.51.100.53:53"},
	})
	if got == nil {
		t.Fatal("resolved source carries no propagation policy")
	}
	if got.Delay != 30*time.Second {
		t.Errorf("delay: got %s, want 30s", got.Delay)
	}
	if got.Timeout != 2*time.Minute {
		t.Errorf("timeout: got %s, want the 2m default", got.Timeout)
	}
	if got.Interval != 5*time.Second {
		t.Errorf("interval: got %s, want the 5s default", got.Interval)
	}
	want := []string{"192.0.2.53:53", "198.51.100.53:53"}
	if !slices.Equal(got.Resolvers, want) {
		t.Errorf("resolvers: got %v, want %v in declaration order", got.Resolvers, want)
	}
}

// TestResolveDNSPropagation_Absent — a DNS-01 source without the option
// resolves to a nil policy, which is what selects the fixed default wait.
func TestResolveDNSPropagation_Absent(t *testing.T) {
	t.Parallel()
	if got := resolvePropagation(t, nil); got != nil {
		t.Errorf("propagation: got %+v, want nil for a source with no policy", got)
	}
}

// TestResolveDNSPropagation_DelayOnly — a delay with no resolvers leaves
// the polling window at zero: there is nothing to poll, so timeout and
// interval would be settings nothing reads.
func TestResolveDNSPropagation_DelayOnly(t *testing.T) {
	t.Parallel()
	got := resolvePropagation(t, &DNSPropagation{Delay: "45s"})
	if got == nil {
		t.Fatal("resolved source carries no propagation policy")
	}
	if got.Delay != 45*time.Second || got.Timeout != 0 || got.Interval != 0 || got.Resolvers != nil {
		t.Errorf("delay-only policy: got %+v", got)
	}
}

// TestResolveDNSPropagation_ResolversOnly — resolvers with no delay drop
// the sleep entirely: polling is the wait.
func TestResolveDNSPropagation_ResolversOnly(t *testing.T) {
	t.Parallel()
	got := resolvePropagation(t, &DNSPropagation{
		Resolvers: []string{"ns.example.test:5353"},
		Timeout:   "90s",
		Interval:  "1s",
	})
	if got == nil {
		t.Fatal("resolved source carries no propagation policy")
	}
	if got.Delay != 0 {
		t.Errorf("delay: got %s, want 0 when polling replaces the sleep", got.Delay)
	}
	if got.Timeout != 90*time.Second || got.Interval != time.Second {
		t.Errorf("polling window: got timeout %s interval %s", got.Timeout, got.Interval)
	}
}

// TestResolveDNSPropagation_Errors walks every validation branch. Each row
// asserts a substring unique to its branch, so a gutted check fails here
// rather than silently passing on the shared prefix.
func TestResolveDNSPropagation_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy DNSPropagation
		want   string
	}{
		{"empty policy", DNSPropagation{}, "waits for nothing"},
		{"zero delay is as empty as none", DNSPropagation{Delay: "0s"}, "waits for nothing"},
		{"delay unparseable", DNSPropagation{Delay: "soon"}, "delay: invalid duration"},
		{"delay negative", DNSPropagation{Delay: "-1s"}, "is negative"},
		{"delay above maximum", DNSPropagation{Delay: "11m"}, "delay 11m0s is above the 10m0s maximum"},
		{"timeout without resolvers", DNSPropagation{Delay: "5s", Timeout: "1m"}, "govern resolver polling"},
		{"interval without resolvers", DNSPropagation{Delay: "5s", Interval: "1s"}, "govern resolver polling"},
		{
			"timeout unparseable",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53"}, Timeout: "soon"},
			"timeout: invalid duration",
		},
		{
			"timeout zero",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53"}, Timeout: "0s"},
			"timeout must be greater than zero",
		},
		{
			"timeout above maximum",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53"}, Timeout: "11m"},
			"timeout 11m0s is above the 10m0s maximum",
		},
		{
			"interval unparseable",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53"}, Interval: "often"},
			"interval: invalid duration",
		},
		{
			"interval below minimum",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53"}, Interval: "50ms"},
			"interval 50ms is below the 100ms minimum",
		},
		{
			"interval above timeout",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53"}, Interval: "3m"},
			"interval 3m0s is above timeout 2m0s",
		},
		{
			"resolver without port",
			DNSPropagation{Resolvers: []string{"192.0.2.53"}},
			`resolver "192.0.2.53" must be host:port`,
		},
		{
			"resolver without host",
			DNSPropagation{Resolvers: []string{":53"}},
			`resolver ":53" has no usable host`,
		},
		{
			"resolver host with leading space",
			DNSPropagation{Resolvers: []string{" 192.0.2.53:53"}},
			`resolver " 192.0.2.53:53" has no usable host`,
		},
		{
			// strconv.Atoi would take "+53"; net.Dial's port parser would
			// not, so it must die here rather than at issuance.
			"resolver port with a sign",
			DNSPropagation{Resolvers: []string{"192.0.2.53:+53"}},
			`resolver "192.0.2.53:+53" has port "+53"`,
		},
		{
			"resolver port not numeric",
			DNSPropagation{Resolvers: []string{"192.0.2.53:domain"}},
			`resolver "192.0.2.53:domain" has port "domain"`,
		},
		{
			"resolver port out of range",
			DNSPropagation{Resolvers: []string{"192.0.2.53:65536"}},
			`resolver "192.0.2.53:65536" has port "65536"`,
		},
		{
			"resolver listed twice",
			DNSPropagation{Resolvers: []string{"192.0.2.53:53", "192.0.2.53:53"}},
			`resolver "192.0.2.53:53" is listed twice`,
		},
		{
			// Same server, different spelling: duplicate detection runs
			// on the canonical form, not the declared bytes.
			"resolver listed twice in another spelling",
			DNSPropagation{Resolvers: []string{"NS.example:53", "ns.example:053"}},
			`resolver "ns.example:053" is listed twice (canonical form ns.example:53)`,
		},
		{
			// IP literals canonicalise too: an expanded IPv6 address and
			// its compressed form are one resolver.
			"resolver listed twice in another IPv6 spelling",
			DNSPropagation{Resolvers: []string{"[2001:db8::1]:53", "[2001:0db8:0:0:0:0:0:1]:53"}},
			`is listed twice (canonical form [2001:db8::1]:53)`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(propagationSource(&c.policy))
			if err == nil {
				t.Fatalf("want error for %+v", c.policy)
			}
			if !strings.Contains(err.Error(), "dns_propagation: ") {
				t.Errorf("error %q lacks the dns_propagation prefix", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

// TestResolveDNSPropagation_RequiresDNS01 — no other challenge publishes a
// DNS record, so a propagation policy on an HTTP-01 or automatic source is
// a setting the runtime would never read.
func TestResolveDNSPropagation_RequiresDNS01(t *testing.T) {
	t.Parallel()
	cfg := tlsRouterConfig(
		AutoTLS("foo.example.test").
			Email("ops@example.test").
			Storage("/var/lib/statute/acme").
			Propagation(DNSPropagation{Delay: "30s"}),
	)
	_, err := Resolve(cfg)
	if err == nil {
		t.Fatal("want error for a propagation policy without CloudflareDNS01")
	}
	if !strings.Contains(err.Error(), "dns_propagation: ") ||
		!strings.Contains(err.Error(), "only a CloudflareDNS01 source") {
		t.Errorf("error: %v", err)
	}
}

// TestPropagation_LastCallWins — like Email and Storage, a second call
// replaces the policy rather than merging into it.
func TestPropagation_LastCallWins(t *testing.T) {
	t.Parallel()
	src := AutoTLS("*.foo.example.test").
		Email("ops@example.test").
		Storage("/var/lib/statute/acme").
		CloudflareDNS01("cf-token").
		Propagation(DNSPropagation{Delay: "10s", Resolvers: []string{"192.0.2.53:53"}}).
		Propagation(DNSPropagation{Delay: "20s"})
	r, err := Resolve(tlsRouterConfig(src))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := r.Listeners[0].AutoTLSSources[0].DNS01.Propagation
	if got.Delay != 20*time.Second || len(got.Resolvers) != 0 {
		t.Errorf("second call did not replace the first: %+v", got)
	}
}

// TestExport_CarriesDNSPropagation — the normalised policy, defaults
// filled, is part of the exported resolved schema.
func TestExport_CarriesDNSPropagation(t *testing.T) {
	t.Parallel()
	cfg := propagationSource(&DNSPropagation{
		Delay:     "30s",
		Resolvers: []string{"192.0.2.53:53", "198.51.100.53:53"},
	})
	var buf bytes.Buffer
	if err := Export(cfg, &buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	var out struct {
		Listeners []struct {
			AutoTLSSources []struct {
				DNS01 *struct {
					Propagation *struct {
						Delay     int64
						Timeout   int64
						Interval  int64
						Resolvers []string
					}
				}
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	dns01 := out.Listeners[0].AutoTLSSources[0].DNS01
	if dns01 == nil || dns01.Propagation == nil {
		t.Fatalf("export carries no propagation policy:\n%s", buf.String())
	}
	p := dns01.Propagation
	if time.Duration(p.Delay) != 30*time.Second ||
		time.Duration(p.Timeout) != 2*time.Minute ||
		time.Duration(p.Interval) != 5*time.Second {
		t.Errorf("durations: delay=%v timeout=%v interval=%v",
			time.Duration(p.Delay), time.Duration(p.Timeout), time.Duration(p.Interval))
	}
	want := []string{"192.0.2.53:53", "198.51.100.53:53"}
	if !slices.Equal(p.Resolvers, want) {
		t.Errorf("resolvers: got %v, want %v", p.Resolvers, want)
	}
}

// TestDNS01_ManagerIssueTimeoutCoversPropagation — one order's deadline
// has to contain the propagation wait it authorises. Leaving it at the
// flat five minutes would cancel a ten-minute policy mid-wait, abandoning
// the authorization after paying for the whole wait.
func TestDNS01_ManagerIssueTimeoutCoversPropagation(t *testing.T) {
	t.Parallel()
	base := func() *resolved.AutoTLS {
		return &resolved.AutoTLS{
			Domains:   []string{"*.foo.example.test"},
			Email:     "ops@example.test",
			Storage:   t.TempDir(),
			Challenge: resolved.ChallengeDNS01,
			DNS01:     &resolved.CloudflareDNS01{APIToken: "cf-token"},
		}
	}

	plain, err := newDNS01Manager(base())
	if err != nil {
		t.Fatalf("newDNS01Manager: %v", err)
	}
	if plain.issueTimeout != acmeIssueTimeout {
		t.Errorf("issueTimeout without a policy: got %s, want %s", plain.issueTimeout, acmeIssueTimeout)
	}

	cfg := base()
	cfg.DNS01.Propagation = &resolved.DNSPropagation{
		Delay:     30 * time.Second,
		Timeout:   4 * time.Minute,
		Interval:  5 * time.Second,
		Resolvers: []string{"192.0.2.53:53"},
	}
	waiting, err := newDNS01Manager(cfg)
	if err != nil {
		t.Fatalf("newDNS01Manager: %v", err)
	}
	want := acmeIssueTimeout + 4*time.Minute + 30*time.Second
	if waiting.issueTimeout != want {
		t.Errorf("issueTimeout with a policy: got %s, want %s", waiting.issueTimeout, want)
	}
	solver, ok := waiting.solver.(*dns01Solver)
	if !ok {
		t.Fatalf("solver: got %T, want *dns01Solver", waiting.solver)
	}
	if solver.prop != cfg.DNS01.Propagation {
		t.Error("propagation policy not threaded into the solver")
	}
}

// stubTXT is a scripted TXT lookup: it counts the queries each resolver
// gets and answers from a per-resolver script.
type stubTXT struct {
	mu      sync.Mutex
	answers map[string][]string // resolver -> the value it eventually serves
	silent  map[string]int      // resolver -> rounds it answers nothing for first
	calls   map[string]int
}

func newStubTXT() *stubTXT {
	return &stubTXT{
		answers: map[string][]string{},
		silent:  map[string]int{},
		calls:   map[string]int{},
	}
}

func (s *stubTXT) lookup(_ context.Context, resolver, _ string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[resolver]++
	if n := s.silent[resolver]; n > 0 {
		s.silent[resolver] = n - 1
		return nil, errors.New("stub: NXDOMAIN")
	}
	return s.answers[resolver], nil
}

func (s *stubTXT) count(resolver string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[resolver]
}

const (
	propRecord = "_acme-challenge.foo.example.test"
	propValue  = "expected-txt-value"
)

// pollingSolver builds a solver polling the named resolvers on a
// millisecond cadence, so the whole suite stays fast.
func pollingSolver(lookup func(context.Context, string, string) ([]string, error), timeout time.Duration, resolvers ...string) *dns01Solver {
	return &dns01Solver{
		prop: &resolved.DNSPropagation{
			Timeout:   timeout,
			Interval:  2 * time.Millisecond,
			Resolvers: resolvers,
		},
		lookupTXT: lookup,
	}
}

// TestDNS01_AwaitPropagation_RequiresEveryResolver — validation waits for
// the slowest resolver, not the first. A resolver that has not served the
// record yet is a resolver the CA might be about to query.
func TestDNS01_AwaitPropagation_RequiresEveryResolver(t *testing.T) {
	t.Parallel()
	stub := newStubTXT()
	stub.answers["fast:53"] = []string{propValue}
	stub.answers["slow:53"] = []string{propValue}
	stub.silent["slow:53"] = 3 // three empty rounds before it converges

	s := pollingSolver(stub.lookup, 2*time.Second, "fast:53", "slow:53")
	if err := s.awaitPropagation(context.Background(), propRecord, propValue); err != nil {
		t.Fatalf("awaitPropagation: %v", err)
	}
	if got := stub.count("slow:53"); got != 4 {
		t.Errorf("laggard queried %d times, want 4 (three empty rounds then the value)", got)
	}
}

// TestDNS01_AwaitPropagation_SatisfiedResolverIsNotRequeried — once a
// resolver has served the value it is dropped from the poll set. Re-asking
// it every round would let a load-balanced anycast resolver that answers
// from a second, colder node undo a result already observed.
func TestDNS01_AwaitPropagation_SatisfiedResolverIsNotRequeried(t *testing.T) {
	t.Parallel()
	stub := newStubTXT()
	stub.answers["fast:53"] = []string{propValue}
	stub.answers["slow:53"] = []string{propValue}
	stub.silent["slow:53"] = 5

	s := pollingSolver(stub.lookup, 2*time.Second, "fast:53", "slow:53")
	if err := s.awaitPropagation(context.Background(), propRecord, propValue); err != nil {
		t.Fatalf("awaitPropagation: %v", err)
	}
	if got := stub.count("fast:53"); got != 1 {
		t.Errorf("satisfied resolver queried %d times, want exactly 1", got)
	}
	if got := stub.count("slow:53"); got != 6 {
		t.Errorf("laggard queried %d times, want 6", got)
	}
}

// TestDNS01_AwaitPropagation_MismatchedValueDoesNotSatisfy — a stale TXT
// record from a previous order is an answer, not the answer.
func TestDNS01_AwaitPropagation_MismatchedValueDoesNotSatisfy(t *testing.T) {
	t.Parallel()
	stub := newStubTXT()
	stub.answers["stale:53"] = []string{"a-previous-orders-value"}

	s := pollingSolver(stub.lookup, 40*time.Millisecond, "stale:53")
	err := s.awaitPropagation(context.Background(), propRecord, propValue)
	if err == nil {
		t.Fatal("want a deadline error when only a stale value is served")
	}
	if !strings.Contains(err.Error(), "stale:53") {
		t.Errorf("error does not name the resolver: %v", err)
	}
}

// TestDNS01_Validate_DeadlineFailsWithoutAskingTheCA — the whole point of
// checking propagation ourselves. When the record never lands, issuance
// fails with an error naming the record and the laggards, and the CA is
// never asked to validate: a failed validation would spend one of the five
// Let's Encrypt allows per hostname per hour, and this costs nothing.
func TestDNS01_Validate_DeadlineFailsWithoutAskingTheCA(t *testing.T) {
	t.Parallel()
	stub := newStubTXT()
	stub.answers["up:53"] = []string{propValue}
	// down:53 never answers.

	s := pollingSolver(stub.lookup, 40*time.Millisecond, "up:53", "down:53")
	fake := newFakeACME(t, nil)
	client, ch := fakeDNS01Challenge(t, fake)

	err := s.validate(context.Background(), client, fake.url("/authz/1"), ch, propRecord, propValue)
	if err == nil {
		t.Fatal("want an error when the record never propagates")
	}
	if !strings.Contains(err.Error(), propRecord) {
		t.Errorf("error does not name the record: %v", err)
	}
	if !strings.Contains(err.Error(), "down:53") {
		t.Errorf("error does not name the unsatisfied resolver: %v", err)
	}
	if strings.Contains(err.Error(), "up:53") {
		t.Errorf("error names a resolver that did serve the record: %v", err)
	}
	if n := fake.count("/chal/dns"); n != 0 {
		t.Errorf("challenge endpoint hit %d times; the CA must never be asked to validate", n)
	}
}

// TestDNS01_Validate_DelayOnlyModeWaitsThenValidates — the delay-only
// mode is the fixed default with a duration of your own: it sleeps, then
// drives the challenge to a validated authorization.
func TestDNS01_Validate_DelayOnlyModeWaitsThenValidates(t *testing.T) {
	t.Parallel()
	const delay = 40 * time.Millisecond
	s := &dns01Solver{prop: &resolved.DNSPropagation{Delay: delay}}
	fake := newFakeACME(t, nil)
	client, ch := fakeDNS01Challenge(t, fake)

	start := time.Now()
	if err := s.validate(context.Background(), client, fake.url("/authz/1"), ch, propRecord, propValue); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("validate returned after %s, before the %s delay elapsed", elapsed, delay)
	}
	if n := fake.count("/chal/dns"); n != 1 {
		t.Errorf("challenge accepted %d times, want exactly 1", n)
	}
}

// TestDNS01_AwaitPropagation_CancelledContextReturnsPromptly — a manager
// shutting down mid-poll must not be held for the policy's timeout.
func TestDNS01_AwaitPropagation_CancelledContextReturnsPromptly(t *testing.T) {
	t.Parallel()
	stub := newStubTXT() // answers nothing, ever
	s := pollingSolver(stub.lookup, time.Minute, "never:53")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := s.awaitPropagation(ctx, propRecord, propValue)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("returned after %s; cancellation must not wait out the timeout", elapsed)
	}
}

// TestDNS01_AwaitPropagation_NoPolicyUsesTheFixedDelay — the default path
// is unchanged: no policy means the fixed wait, honouring cancellation.
func TestDNS01_AwaitPropagation_NoPolicyUsesTheFixedDelay(t *testing.T) {
	t.Parallel()
	if dns01PropagationDelay != 15*time.Second {
		t.Errorf("default wait changed to %s; it is documented as 15s", dns01PropagationDelay)
	}
	s := &dns01Solver{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.awaitPropagation(ctx, propRecord, propValue); !errors.Is(err, context.Canceled) {
		t.Errorf("error: got %v, want context.Canceled", err)
	}
	// The use site actually sleeps the constant: a context that expires
	// after 30ms must interrupt the wait. If this returned nil, the
	// default path slept something far shorter than dns01PropagationDelay.
	tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer tcancel()
	if err := s.awaitPropagation(tctx, propRecord, propValue); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error: got %v, want context.DeadlineExceeded from inside the fixed wait", err)
	}
}

// TestResolveDNSPropagation_CanonicalisesResolvers — the resolved schema
// stores each resolver canonically: hostname lowercased, port in plain
// decimal, IPv6 bracket form preserved.
func TestResolveDNSPropagation_CanonicalisesResolvers(t *testing.T) {
	t.Parallel()
	got := resolvePropagation(t, &DNSPropagation{
		Resolvers: []string{"NS.EXAMPLE.test:0053", "[2001:0db8:0:0:0:0:0:1]:53"},
	})
	want := []string{"ns.example.test:53", "[2001:db8::1]:53"}
	if !slices.Equal(got.Resolvers, want) {
		t.Errorf("resolvers: got %v, want the canonical %v", got.Resolvers, want)
	}
}

// TestResolveDNSPropagation_ShortTimeoutClampsDefaultInterval — a timeout
// below the 5s default cadence asks for fail-fast polling; the default
// interval clamps down to it instead of erroring about a field the user
// never wrote. An explicit interval above the timeout still errors.
func TestResolveDNSPropagation_ShortTimeoutClampsDefaultInterval(t *testing.T) {
	t.Parallel()
	got := resolvePropagation(t, &DNSPropagation{
		Resolvers: []string{"192.0.2.53:53"},
		Timeout:   "3s",
	})
	if got.Timeout != 3*time.Second || got.Interval != 3*time.Second {
		t.Errorf("window: got timeout %s interval %s, want both 3s", got.Timeout, got.Interval)
	}
}

// TestDNS01_AwaitPropagation_DelayPrecedesPolling — the issue's combined
// shape at runtime: the fixed delay elapses before the first resolver
// query is made, and the flow then proceeds through polling to success.
func TestDNS01_AwaitPropagation_DelayPrecedesPolling(t *testing.T) {
	t.Parallel()
	const delay = 40 * time.Millisecond
	stub := newStubTXT()
	stub.answers["r:53"] = []string{propValue}
	var (
		mu    sync.Mutex
		first time.Time
	)
	s := &dns01Solver{
		prop: &resolved.DNSPropagation{
			Delay:     delay,
			Timeout:   2 * time.Second,
			Interval:  2 * time.Millisecond,
			Resolvers: []string{"r:53"},
		},
		lookupTXT: func(ctx context.Context, resolver, name string) ([]string, error) {
			mu.Lock()
			if first.IsZero() {
				first = time.Now()
			}
			mu.Unlock()
			return stub.lookup(ctx, resolver, name)
		},
	}
	start := time.Now()
	if err := s.awaitPropagation(context.Background(), propRecord, propValue); err != nil {
		t.Fatalf("awaitPropagation: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if first.IsZero() {
		t.Fatal("no resolver was ever queried")
	}
	if elapsed := first.Sub(start); elapsed < delay {
		t.Errorf("first query after %s; the %s delay must elapse first", elapsed, delay)
	}
}

// TestDNS01_AwaitPropagation_FirstRoundIsImmediate — with a large interval
// and an already-propagated record, polling succeeds at once. Waiting one
// tick before the first round would run into the 3s context instead.
func TestDNS01_AwaitPropagation_FirstRoundIsImmediate(t *testing.T) {
	t.Parallel()
	stub := newStubTXT()
	stub.answers["r:53"] = []string{propValue}
	s := &dns01Solver{
		prop: &resolved.DNSPropagation{
			Timeout:   30 * time.Second,
			Interval:  25 * time.Second,
			Resolvers: []string{"r:53"},
		},
		lookupTXT: stub.lookup,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.awaitPropagation(ctx, propRecord, propValue); err != nil {
		t.Fatalf("awaitPropagation: %v — the first round must not wait an interval", err)
	}
}

// TestDNS01_AwaitPropagation_BlackholeDoesNotStarveOthers — a resolver
// that consumes its whole probe budget, listed first, with Timeout equal
// to Interval: probed sequentially it would spend the entire polling
// window and the healthy resolver after it would only ever see an expired
// context, so the deadline error would blame both and swap with
// declaration order. Rounds probe concurrently, so the healthy resolver
// is satisfied in the first round and the error names only the black
// hole.
func TestDNS01_AwaitPropagation_BlackholeDoesNotStarveOthers(t *testing.T) {
	t.Parallel()
	const window = 150 * time.Millisecond
	s := &dns01Solver{
		prop: &resolved.DNSPropagation{
			Timeout:   window,
			Interval:  window,
			Resolvers: []string{"blackhole:53", "healthy:53"},
		},
		lookupTXT: func(ctx context.Context, resolver, _ string) ([]string, error) {
			switch resolver {
			case "blackhole:53":
				<-ctx.Done()
				return nil, ctx.Err()
			case "healthy:53":
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return []string{propValue}, nil
			}
			panic("unexpected resolver " + resolver)
		},
	}
	err := s.awaitPropagation(context.Background(), propRecord, propValue)
	if err == nil {
		t.Fatal("want a deadline error naming the black hole")
	}
	if !strings.Contains(err.Error(), "blackhole:53") {
		t.Errorf("error %q does not name the black hole", err)
	}
	if strings.Contains(err.Error(), "healthy:53") {
		t.Errorf("error %q blames the healthy resolver; it served the value and its probe must not wait behind the black hole's", err)
	}
}

// deadlineSolver captures how much time the order context it is handed
// still has, then fails the order so the test ends there.
type deadlineSolver struct {
	mu        sync.Mutex
	remaining time.Duration
}

func (*deadlineSolver) challengeType() string { return "dns-01" }

func (s *deadlineSolver) satisfy(ctx context.Context, _ *acme.Client, _, _ string, _ *acme.Challenge) error {
	if dl, ok := ctx.Deadline(); ok {
		s.mu.Lock()
		s.remaining = time.Until(dl)
		s.mu.Unlock()
	}
	return errors.New("deadline captured; stop the order here")
}

// TestDNS01_IssueTimeoutReachesTheOrderContext — the per-order context is
// built from the manager's extended issueTimeout, not the flat constant: a
// solver inside an order authorised to wait 18 minutes must see far more
// than five minutes on its clock.
func TestDNS01_IssueTimeoutReachesTheOrderContext(t *testing.T) {
	t.Parallel()
	solver := &deadlineSolver{}
	m, err := newACMEManager(&resolved.AutoTLS{
		Domains:   []string{"budget.example"},
		Email:     "ops@budget.example",
		Storage:   t.TempDir(),
		Challenge: resolved.ChallengeDNS01,
		DNS01: &resolved.CloudflareDNS01{
			APIToken: "cf-token",
			Propagation: &resolved.DNSPropagation{
				Delay:     8 * time.Minute,
				Timeout:   10 * time.Minute,
				Interval:  5 * time.Second,
				Resolvers: []string{"192.0.2.53:53"},
			},
		},
	}, "dns01", solver)
	if err != nil {
		t.Fatalf("newACMEManager: %v", err)
	}
	fake := newFakeACME(t, nil)
	m.directoryURL = fake.url("/dir")
	run, err := m.start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer run.stop()
	if _, err := m.getOrIssue(context.Background(), "budget.example"); err == nil {
		t.Fatal("getOrIssue: want the solver's deliberate error")
	}
	solver.mu.Lock()
	defer solver.mu.Unlock()
	if solver.remaining == 0 {
		t.Fatal("solver never saw an order deadline")
	}
	if solver.remaining <= acmeIssueTimeout+10*time.Minute {
		t.Errorf("order deadline %s away; the 18m propagation budget was not added to the flat %s", solver.remaining, acmeIssueTimeout)
	}
}

// fakeDNS01Challenge wires an ACME client to the fake CA and returns the
// DNS-01 challenge it offers.
func fakeDNS01Challenge(t *testing.T, fake *fakeACME) (*acme.Client, *acme.Challenge) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &acme.Client{Key: key, DirectoryURL: fake.url("/dir")}
	// Register first so the client carries an account KID: without one
	// every POST re-derives it through an endpoint the fake answers with
	// 201, which x/crypto/acme retries forever.
	if _, err := client.Register(context.Background(), &acme.Account{}, acme.AcceptTOS); err != nil {
		t.Fatalf("register with the fake CA: %v", err)
	}
	ch := &acme.Challenge{Type: "dns-01", URI: fake.url("/chal/dns"), Token: fakeACMEToken, Status: acme.StatusPending}
	return client, ch
}

// TestDNS01_LookupTXTAt_QueriesTheGivenResolver drives the real default
// lookup — net.Resolver with the custom dialer — against an in-process UDP
// DNS server, so the dial path that production uses is exercised rather
// than assumed.
func TestDNS01_LookupTXTAt_QueriesTheGivenResolver(t *testing.T) {
	t.Parallel()
	addr := startTXTServer(t, propRecord+".", []string{propValue})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := lookupTXTAt(ctx, addr, propRecord)
	if err != nil {
		t.Fatalf("lookupTXTAt: %v", err)
	}
	if !slices.Contains(got, propValue) {
		t.Errorf("TXT records: got %v, want one equal to %q", got, propValue)
	}

	// And the same path drives the solver's satisfaction check.
	s := pollingSolver(nil, 2*time.Second, addr)
	if err := s.awaitPropagation(ctx, propRecord, propValue); err != nil {
		t.Errorf("awaitPropagation over the real lookup: %v", err)
	}
}

// startTXTServer runs a single-purpose UDP DNS server on 127.0.0.1 that
// answers TXT queries for wantName (an absolute name) with values, and
// NXDOMAIN for everything else — including the search-suffixed names the
// host's resolv.conf may make the stub resolver try. It returns the
// "host:port" to point a resolver at.
func startTXTServer(t *testing.T, wantName string, values []string) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, from, rerr := conn.ReadFrom(buf)
			if rerr != nil {
				return // the listener was closed by cleanup
			}
			reply, berr := buildTXTReply(buf[:n], wantName, values)
			if berr != nil {
				continue
			}
			if _, werr := conn.WriteTo(reply, from); werr != nil {
				return
			}
		}
	}()
	return conn.LocalAddr().String()
}

// buildTXTReply parses one DNS query and packs the matching response.
func buildTXTReply(query []byte, wantName string, values []string) ([]byte, error) {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	var msg dnsmessage.Message
	msg.ID = hdr.ID
	msg.Response = true
	msg.Authoritative = true
	msg.RecursionDesired = hdr.RecursionDesired
	msg.RecursionAvailable = true
	msg.Questions = []dnsmessage.Question{q}
	if q.Type == dnsmessage.TypeTXT && q.Name.String() == wantName {
		msg.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name:  q.Name,
				Type:  dnsmessage.TypeTXT,
				Class: dnsmessage.ClassINET,
				TTL:   60,
			},
			Body: &dnsmessage.TXTResource{TXT: values},
		}}
	} else {
		msg.RCode = dnsmessage.RCodeNameError
	}
	return msg.Pack()
}
