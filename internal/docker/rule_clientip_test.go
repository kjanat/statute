package docker

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestClientIPIntersectionKeepsNarrowerSharedBase(t *testing.T) {
	for _, networks := range [][2]string{{"10.0.0.0/16", "10.0.0.0/8"}, {"2001:db8::/64", "2001:db8::/32"}} {
		for _, reverse := range []bool{false, true} {
			left, right := networks[0], networks[1]
			if reverse {
				left, right = right, left
			}
			ms, err := ParseRule("ClientIP(`" + left + "`) && ClientIP(`" + right + "`)")
			if err != nil {
				t.Fatal(err)
			}
			if len(ms) != 1 || len(ms[0].PeerIPCIDRs) != 1 {
				t.Fatalf("matchers=%+v", ms)
			}
			got := netip.MustParsePrefix(ms[0].PeerIPCIDRs[0])
			if got.String() != networks[0] {
				t.Fatalf("intersection=%s want=%s", got, networks[0])
			}
		}
	}
}

func TestClientIPConjunctionIntersectsNetworks(t *testing.T) {
	for _, rule := range []string{
		"ClientIP(`10.0.0.0/8`) && ClientIP(`10.1.0.0/16`)",
		"ClientIP(`10.1.0.0/16`) && ClientIP(`10.0.0.0/8`)",
		"Host(`app.example`) && ClientIP(`10.0.0.0/8`) && ClientIP(`10.1.2.3/16`)",
	} {
		matchers, err := ParseRule(rule)
		if err != nil {
			t.Fatal(err)
		}
		if len(matchers) != 1 || !reflect.DeepEqual(matchers[0].PeerIPCIDRs, []string{"10.1.0.0/16"}) {
			t.Fatalf("network intersection=%+v", matchers)
		}
	}
}

func TestClientIPRuleRejectsInvalidOrDisjointConstraints(t *testing.T) {
	for _, rule := range []string{
		"ClientIP(`invalid`)", "ClientIP(`10.0.0.0/8`) && ClientIP(`192.0.2.0/24`)",
	} {
		if _, err := ParseRule(rule); err == nil {
			t.Fatalf("invalid peer constraint accepted: %s", rule)
		}
	}
}
