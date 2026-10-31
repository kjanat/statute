package statute

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"statute.kjanat.dev/resolved"
)

type authoritativeZoneFixture struct {
	broken string
	checks atomic.Int32
}

func (f *authoritativeZoneFixture) query(ctx context.Context, server, name string, kind uint16, recursive bool) (*dns.Msg, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.broken == "offline" {
		return nil, errors.New("resolver offline")
	}
	if !recursive {
		if strings.HasPrefix(server, "192.0.2.10:") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return f.txt(server), nil
	}
	switch kind {
	case dns.TypeCNAME:
		return f.cname(name), nil
	case dns.TypeSOA:
		return f.soa(name), nil
	case dns.TypeNS:
		return f.ns()
	case dns.TypeA, dns.TypeAAAA:
		return f.address(name, kind)
	default:
		return &dns.Msg{}, nil
	}
}

func (f *authoritativeZoneFixture) cname(name string) *dns.Msg {
	reply := &dns.Msg{}
	if name == "_acme-challenge.example." || f.broken == "loop" {
		reply.Answer = []dns.RR{&dns.CNAME{Hdr: dns.RR_Header{Name: name}, Target: "_acme-challenge.delegated.test."}}
	}
	return reply
}

func (f *authoritativeZoneFixture) soa(name string) *dns.Msg {
	reply := &dns.Msg{}
	if name == "delegated.test." && f.broken != "no zone" {
		reply.Ns = []dns.RR{&dns.SOA{Hdr: dns.RR_Header{Name: "delegated.test."}}}
	}
	return reply
}

func (f *authoritativeZoneFixture) ns() (*dns.Msg, error) {
	if f.broken == "no servers" {
		return &dns.Msg{}, nil
	}
	if f.broken == "NS error" {
		return nil, errors.New("NS unavailable")
	}
	return &dns.Msg{Answer: []dns.RR{&dns.NS{Ns: "ns1.test."}, &dns.NS{Ns: "ns2.test."}}}, nil
}

func (f *authoritativeZoneFixture) address(name string, kind uint16) (*dns.Msg, error) {
	if kind == dns.TypeAAAA {
		return nil, errors.New("no IPv6 answer")
	}
	reply := &dns.Msg{}
	if f.broken == "no addresses" {
		return reply, nil
	}
	address := "192.0.2.1"
	if name == "ns2.test." {
		address = "192.0.2.2"
	}
	reply.Answer = []dns.RR{&dns.A{A: net.ParseIP(address)}}
	if f.broken == "black hole" && name == "ns1.test." {
		reply.Answer = append([]dns.RR{&dns.A{A: net.ParseIP("192.0.2.10")}}, reply.Answer...)
	}
	return reply, nil
}

func (f *authoritativeZoneFixture) txt(server string) *dns.Msg {
	f.checks.Add(1)
	value := "propagated"
	if f.broken == "mismatch" && strings.HasPrefix(server, "192.0.2.2:") {
		value = "previous"
	}
	reply := &dns.Msg{Answer: []dns.RR{&dns.TXT{Txt: []string{value}}}}
	reply.Authoritative = f.broken != "not authoritative"
	return reply
}

func TestAuthoritativeDNSRequiresEveryNameserver(t *testing.T) {
	for _, broken := range []string{"", "offline", "loop", "no zone", "no servers", "NS error", "no addresses", "mismatch", "not authoritative"} {
		t.Run(broken, func(t *testing.T) {
			fixture := &authoritativeZoneFixture{broken: broken}
			solver := &dns01Solver{queryDNS: fixture.query, prop: &resolved.DNSPropagation{DiscoveryResolvers: []string{"recursive:53"}}}
			visible := solver.authoritativeVisible(t.Context(), "_acme-challenge.example.", "propagated")
			if visible != (broken == "") {
				t.Fatalf("authoritative visibility=%t", visible)
			}
			if broken == "" && fixture.checks.Load() != 2 {
				t.Fatal("did not verify both authoritative servers")
			}
		})
	}
}

func TestAuthoritativeDNSPollingDeadline(t *testing.T) {
	for _, broken := range []string{"", "mismatch"} {
		fixture := &authoritativeZoneFixture{broken: broken}
		solver := &dns01Solver{queryDNS: fixture.query, prop: &resolved.DNSPropagation{Authoritative: true, Timeout: 30 * time.Millisecond, Interval: 10 * time.Millisecond, DiscoveryResolvers: []string{"recursive:53"}}}
		err := solver.pollAuthoritative(t.Context(), "_acme-challenge.example.", "propagated")
		if broken == "" && err != nil {
			t.Fatal(err)
		}
		if broken != "" && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline=%v", err)
		}
	}
}

func TestAuthoritativeDNSUnresponsiveAddressKeepsAlternatives(t *testing.T) {
	fixture := &authoritativeZoneFixture{broken: "black hole"}
	solver := &dns01Solver{queryDNS: fixture.query, prop: &resolved.DNSPropagation{Authoritative: true, Timeout: time.Second, Interval: 50 * time.Millisecond, DiscoveryResolvers: []string{"recursive:53"}}}
	if err := solver.pollAuthoritative(t.Context(), "_acme-challenge.example.", "propagated"); err != nil {
		t.Fatal(err)
	}
	if fixture.checks.Load() != 2 {
		t.Fatalf("authoritative checks=%d", fixture.checks.Load())
	}
}

func TestDNSDiscoveryResolverSelection(t *testing.T) {
	solver := &dns01Solver{prop: &resolved.DNSPropagation{Resolvers: []string{"recursive:53"}}}
	if got := solver.recursiveServers(); len(got) != 1 || got[0] != "recursive:53" {
		t.Fatal("explicit resolver lost")
	}
	solver.prop = &resolved.DNSPropagation{}
	if len(solver.recursiveServers()) == 0 {
		t.Fatal("system resolver discovery returned nothing")
	}
	solver.prop.Interval = 0
	if err := solver.pollAuthoritative(t.Context(), "example.", "value"); err == nil {
		t.Fatal("zero polling interval accepted")
	}
}

func TestDNSPropagationVerifiesAuthoritativeThenAllRecursive(t *testing.T) {
	fixture := &authoritativeZoneFixture{}
	var recursive atomic.Int32
	solver := &dns01Solver{queryDNS: fixture.query, prop: &resolved.DNSPropagation{Authoritative: true, RequireAllRecursive: true, Timeout: time.Second, Interval: 10 * time.Millisecond, DiscoveryResolvers: []string{"recursive1:53", "recursive2:53"}}, lookupTXT: func(context.Context, string, string) ([]string, error) {
		if fixture.checks.Load() != 2 {
			t.Error("recursive verification ran before authoritative verification")
		}
		recursive.Add(1)
		return []string{"propagated"}, nil
	}}
	if err := solver.awaitPropagation(t.Context(), "_acme-challenge.example.", "propagated"); err != nil {
		t.Fatal(err)
	}
	if recursive.Load() != 2 {
		t.Fatal("not every recursive resolver was verified")
	}
	solver.prop.Authoritative = false
	solver.prop.RequireAllRecursive = false
	if err := solver.awaitPropagation(t.Context(), "example.", "value"); err != nil {
		t.Fatal(err)
	}
}
