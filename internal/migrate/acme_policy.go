package migrate

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	statute "statute.kjanat.dev"
)

const dnsChallengeBase = "acme.dnsChallenge"

func (g *generator) acmePropagation(o *Object, a *statute.AutoTLSConfig) string {
	if o.text(dnsChallengeBase+".provider") != "cloudflare" {
		return ""
	}
	resolvers, ok := sourceDNSResolvers(o)
	if !ok {
		return ""
	}
	delay, ok := sourceDNSDelay(o)
	if !ok {
		return ""
	}
	disabled, noANS, allRNS, valid := sourceDNSChecks(o)
	if !valid {
		return ""
	}
	p := statute.DNSPropagation{Delay: delay, Authoritative: !noANS, DiscoveryResolvers: resolvers, DisableChecks: disabled}
	p.RequireAllRecursive = allRNS
	if allRNS {
		p.Resolvers = resolvers
	}
	if disabled {
		p.Authoritative = false
	}
	a.Propagation(p)
	classifyDNSChecks(o)
	return fmt.Sprintf(".Propagation(statute.DNSPropagation{Delay:%q,Authoritative:%t,DiscoveryResolvers:[]string{%s},Resolvers:[]string{%s},DisableChecks:%t,RequireAllRecursive:%t})", delay, p.Authoritative, quoted(p.DiscoveryResolvers), quoted(p.Resolvers), disabled, allRNS)
}

func sourceDNSResolvers(o *Object) ([]string, bool) {
	path := dnsChallengeBase + ".resolvers"
	if o.value(path) == nil {
		return nil, true
	}
	values, ok := stringList(o.value(path), false)
	if !ok {
		o.mark(path, Manual, "DNS resolvers require literal IP or host:port addresses")
		return nil, false
	}
	var out []string
	for _, value := range values {
		resolver, ok := nativeDNSResolver(value)
		if !ok {
			o.mark(path, Manual, "native DNS resolvers require IP or host:port addresses rather than resolver URLs")
			return nil, false
		}
		out = append(out, resolver)
	}
	o.mark(path, Translated, "native DNS-01 resolver verification candidate")
	return out, true
}

func nativeDNSResolver(value string) (string, bool) {
	if ip, err := netip.ParseAddr(value); err == nil {
		return net.JoinHostPort(ip.String(), "53"), true
	}
	if !strings.Contains(value, ":") {
		return net.JoinHostPort(value, "53"), true
	}
	if _, _, err := net.SplitHostPort(value); err != nil {
		return "", false
	}
	return value, true
}

func sourceDNSDelay(o *Object) (string, bool) {
	path := dnsChallengeBase + ".propagation.delayBeforeChecks"
	legacy := dnsChallengeBase + ".delayBeforeCheck"
	if o.value(path) == nil {
		path = legacy
	} else if o.value(legacy) != nil {
		o.mark(legacy, Manual, "both legacy and current propagation delay are configured; choose one delay")
	}
	if o.value(path) == nil {
		return "", true
	}
	delay := o.text(path)
	duration, err := time.ParseDuration(delay)
	if err != nil || duration < 0 || duration > 10*time.Minute {
		o.mark(path, Manual, "native propagation delay must be a literal duration from zero through ten minutes")
		return "", false
	}
	o.mark(path, Translated, "fixed delay before DNS-01 propagation checks")
	return delay, true
}

func classifyDNSChecks(o *Object) {
	paths := []string{dnsChallengeBase + ".disablePropagationCheck", dnsChallengeBase + ".propagation.disableChecks", dnsChallengeBase + ".propagation.disableANSChecks", dnsChallengeBase + ".propagation.requireAllRNS"}
	for _, path := range paths {
		if o.value(path) == nil {
			continue
		}
		if _, ok := sourceBoolValue(o.value(path), false); !ok {
			o.mark(path, Manual, "DNS propagation switch requires a literal boolean")
			continue
		}
		o.mark(path, Translated, "source DNS check mode covered by the propagation decision")
	}
}

func sourceDNSChecks(o *Object) (bool, bool, bool, bool) {
	disabled, valid := sourceBoolValue(o.value(dnsChallengeBase+".propagation.disableChecks"), false)
	legacy, legacyOK := sourceBoolValue(o.value(dnsChallengeBase+".disablePropagationCheck"), false)
	noANS, ansOK := sourceBoolValue(o.value(dnsChallengeBase+".propagation.disableANSChecks"), false)
	allRNS, rnsOK := sourceBoolValue(o.value(dnsChallengeBase+".propagation.requireAllRNS"), false)
	if !valid || !legacyOK || !ansOK || !rnsOK {
		o.mark(dnsChallengeBase, Manual, "invalid DNS propagation switch")
		return false, false, false, false
	}
	disabled = disabled || legacy || noANS && !allRNS
	return disabled, noANS, allRNS, true
}
