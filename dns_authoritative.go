package statute

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

func (s *dns01Solver) pollAuthoritative(ctx context.Context, name, value string) error {
	ctx, cancel := context.WithTimeout(ctx, s.prop.Timeout)
	defer cancel()
	interval := s.prop.Interval
	if interval <= 0 {
		return errors.New("dns01: authoritative checks require a positive interval")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		round, cancel := context.WithTimeout(ctx, interval)
		visible := s.authoritativeVisible(round, dns.Fqdn(name), value)
		cancel()
		if visible {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("dns01: authoritative TXT verification: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *dns01Solver) recursiveServers() []string {
	if len(s.prop.DiscoveryResolvers) > 0 {
		return s.prop.DiscoveryResolvers
	}
	if len(s.prop.Resolvers) > 0 {
		return s.prop.Resolvers
	}
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil || len(config.Servers) == 0 {
		return []string{"127.0.0.1:53"}
	}
	out := make([]string, 0, len(config.Servers))
	for _, server := range config.Servers {
		out = append(out, net.JoinHostPort(server, config.Port))
	}
	return out
}

func dnsExchange(ctx context.Context, server, name string, kind uint16, recursive bool) (*dns.Msg, error) {
	message := new(dns.Msg)
	message.SetQuestion(dns.Fqdn(name), kind)
	message.RecursionDesired = recursive
	client := &dns.Client{Net: "udp"}
	reply, _, err := client.ExchangeContext(ctx, message, server)
	if err == nil && reply.Truncated {
		client.Net = networkTCP
		reply, _, err = client.ExchangeContext(ctx, message, server)
	}
	if err != nil {
		return nil, err
	}
	if reply.Rcode != dns.RcodeSuccess && reply.Rcode != dns.RcodeNameError {
		return nil, errors.New("DNS query failed")
	}
	return reply, nil
}

func (s *dns01Solver) recursiveQuery(ctx context.Context, name string, kind uint16) (*dns.Msg, error) {
	ctx, cancel := context.WithCancel(ctx)
	servers := s.recursiveServers()
	answers := make(chan *dns.Msg, len(servers))
	var queries sync.WaitGroup
	defer func() { cancel(); queries.Wait() }()
	for _, server := range servers {
		queries.Go(func() {
			reply, err := s.exchangeDNS(ctx, server, name, kind, true)
			if err != nil {
				reply = nil
			}
			answers <- reply
		})
	}
	for range servers {
		select {
		case reply := <-answers:
			if reply != nil {
				return reply, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("no recursive resolver answered")
}

func (s *dns01Solver) authoritativeVisible(ctx context.Context, name, value string) bool {
	name = s.delegatedChallenge(ctx, name)
	if name == "" {
		return false
	}
	zone := s.authoritativeZone(ctx, name)
	if zone == "" {
		return false
	}
	response, err := s.recursiveQuery(ctx, zone, dns.TypeNS)
	if err != nil {
		return false
	}
	servers := nameservers(response)
	if len(servers) == 0 {
		return false
	}
	for _, server := range servers {
		if !s.authoritativeTXT(ctx, server, name, value) {
			return false
		}
	}
	return true
}

func (s *dns01Solver) delegatedChallenge(ctx context.Context, name string) string {
	for range 16 {
		response, err := s.recursiveQuery(ctx, name, dns.TypeCNAME)
		if err != nil {
			return ""
		}
		target := ""
		for _, rr := range response.Answer {
			if record, ok := rr.(*dns.CNAME); ok && strings.EqualFold(record.Hdr.Name, name) {
				target = record.Target
				break
			}
		}
		if target == "" {
			return name
		}
		name = target
	}
	return ""
}

func (s *dns01Solver) authoritativeZone(ctx context.Context, name string) string {
	for candidate := name; candidate != "."; {
		response, err := s.recursiveQuery(ctx, candidate, dns.TypeSOA)
		if err == nil {
			for _, rr := range append(response.Answer, response.Ns...) {
				if record, ok := rr.(*dns.SOA); ok {
					return record.Hdr.Name
				}
			}
		}
		_, rest, found := strings.Cut(candidate, ".")
		if !found || rest == "" {
			break
		}
		candidate = rest
	}
	return ""
}

func nameservers(response *dns.Msg) []string {
	var servers []string
	for _, rr := range append(response.Answer, response.Ns...) {
		if record, ok := rr.(*dns.NS); ok {
			servers = append(servers, record.Ns)
		}
	}
	return servers
}

func (s *dns01Solver) authoritativeTXT(ctx context.Context, server, name, value string) bool {
	for _, address := range s.nameserverAddresses(ctx, server) {
		response, err := s.exchangeDNS(ctx, net.JoinHostPort(address, "53"), name, dns.TypeTXT, false)
		if err != nil || !response.Authoritative {
			continue
		}
		for _, rr := range response.Answer {
			if record, ok := rr.(*dns.TXT); ok && strings.Join(record.Txt, "") == value {
				return true
			}
		}
	}
	return false
}

func (s *dns01Solver) exchangeDNS(ctx context.Context, server, name string, kind uint16, recursive bool) (*dns.Msg, error) {
	if s.queryDNS != nil {
		return s.queryDNS(ctx, server, name, kind, recursive)
	}
	return dnsExchange(ctx, server, name, kind, recursive)
}

func (s *dns01Solver) nameserverAddresses(ctx context.Context, name string) []string {
	var addresses []string
	for _, kind := range []uint16{dns.TypeA, dns.TypeAAAA} {
		response, err := s.recursiveQuery(ctx, name, kind)
		if err != nil {
			continue
		}
		for _, record := range response.Answer {
			switch record := record.(type) {
			case *dns.A:
				addresses = append(addresses, record.A.String())
			case *dns.AAAA:
				addresses = append(addresses, record.AAAA.String())
			}
		}
	}
	return addresses
}
