package migrate

import (
	"fmt"
	"strconv"

	statute "statute.kjanat.dev"
)

func sourceIPSelection(o *Object, base string) (statute.IPSelection, string, bool) {
	p := statute.IPSelection{Mode: "peer"}
	if value := o.value(base + ".depth"); value != nil {
		n, err := integer(value)
		if err != nil || n < 0 {
			o.mark(base+".depth", Manual, "IP depth must be a nonnegative integer")
			return p, "", false
		}
		p.Depth = n
	}
	if value := o.value(base + ".excludedIPs"); value != nil {
		values, ok := stringList(value, o.Provider == providerDocker)
		if !ok {
			o.mark(base+".excludedIPs", Manual, "excluded IPs require literal addresses or CIDRs")
			return p, "", false
		}
		p.ExcludedIPs = values
	}
	subnet, ok := sourceIPv6Subnet(o, base, &p)
	if !ok {
		return p, "", false
	}
	if p.Depth > 0 || len(p.ExcludedIPs) > 0 {
		p.Mode = "forwarded"
	}
	o.mark(base, Translated, "explicit direct-peer or forwarded-chain address selection")
	return p, fmt.Sprintf("statute.IPSelection{Mode:%q,Depth:%d,ExcludedIPs:[]string{%s},IPv6Subnet:%s}", p.Mode, p.Depth, quoted(p.ExcludedIPs), subnet), true
}

func sourceIPv6Subnet(o *Object, base string, p *statute.IPSelection) (string, bool) {
	subnet := "nil"
	if value := o.value(base + ".ipv6Subnet"); value != nil {
		n, err := integer(value)
		if err != nil {
			return "", false
		}
		if n >= 0 && n <= 128 {
			p.IPv6Subnet = &n
			subnet = "func()*int{v:=" + strconv.Itoa(n) + ";return &v}()"
		}
	}
	return subnet, true
}
