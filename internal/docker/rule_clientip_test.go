package docker

import (
	"reflect"
	"testing"
)

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
