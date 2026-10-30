package statute

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"

	"statute.kjanat.dev/resolved"
)

func TestDNSDiscoveryFallbackAfterBlackhole(t *testing.T) {
	blackhole, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(r)
		reply.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"propagated"}}}
		_ = w.WriteMsg(reply)
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	solver := &dns01Solver{prop: &resolved.DNSPropagation{DiscoveryResolvers: []string{blackhole.LocalAddr().String(), conn.LocalAddr().String()}}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	reply, err := solver.recursiveQuery(ctx, "_acme-challenge.example.", dns.TypeTXT)
	if err != nil || len(reply.Answer) != 1 {
		t.Fatalf("healthy fallback did not answer: %v %v", reply, err)
	}
}
