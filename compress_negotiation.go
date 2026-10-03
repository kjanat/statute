package statute

import (
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const codingGzip = "gzip"

type encodingPreferences struct {
	present bool
	quality map[string]int
}

func parseAcceptEncoding(h http.Header) (encodingPreferences, bool) {
	values, present := cacheHeaderValues(h, "Accept-Encoding")
	p := encodingPreferences{present: present, quality: make(map[string]int)}
	for _, value := range values {
		for member := range strings.SplitSeq(value, ",") {
			member = strings.Trim(member, " \t")
			if member == "" {
				continue
			}
			name, q, ok := parseEncodingMember(member)
			if !ok {
				return p, false
			}
			if previous, exists := p.quality[name]; exists {
				q = min(q, previous)
			}
			p.quality[name] = q
		}
	}
	return p, true
}

func parseEncodingMember(member string) (string, int, bool) {
	name, parameter, weighted := strings.Cut(member, ";")
	name = normalizeCoding(strings.Trim(name, " \t"))
	if !httpguts.ValidHeaderFieldName(name) {
		return "", 0, false
	}
	if !weighted {
		return name, 1000, true
	}
	key, value, ok := strings.Cut(parameter, "=")
	if !ok || !strings.EqualFold(strings.Trim(key, " \t"), "q") {
		return "", 0, false
	}
	q, valid := encodingQuality(strings.Trim(value, " \t"))
	return name, q, valid
}

// Accept a leading-dot fraction as an interoperability extension. Integer
// arithmetic keeps exclusions exact and rejects signs, exponents, and NaN.
func encodingQuality(value string) (int, bool) {
	if value == "." {
		return 0, false
	}
	if strings.HasPrefix(value, ".") {
		value = "0" + value
	}
	whole, fraction, _ := strings.Cut(value, ".")
	if whole != "0" && whole != "1" {
		return 0, false
	}
	return fractionQuality(whole, fraction)
}

func fractionQuality(whole, fraction string) (int, bool) {
	if len(fraction) > 3 {
		return 0, false
	}
	q, scale := 0, 100
	for _, digit := range fraction {
		if digit < '0' || digit > '9' || (whole == "1" && digit != '0') {
			return 0, false
		}
		q += int(digit-'0') * scale
		scale /= 10
	}
	if whole == "1" {
		q = 1000
	}
	return q, true
}

func normalizeCoding(name string) string {
	name = strings.ToLower(name)
	if name == "x-gzip" {
		return codingGzip
	}
	return name
}

func (p encodingPreferences) weight(name string) int {
	name = normalizeCoding(name)
	if !p.present {
		return 1000
	}
	if q, ok := p.quality[name]; ok {
		return q
	}
	wildcard, hasWildcard := p.quality["*"]
	if name == "identity" && (!hasWildcard || wildcard != 0) {
		return 1000
	}
	return wildcard
}

type compressionNegotiator struct {
	prefs                   encodingPreferences
	gzip, brotli, transform bool
}

// selectCoding returns a new coding, or an empty coding to preserve the
// representation. Existing coding stacks must be acceptable in their entirety.
func (n *compressionNegotiator) selectCoding(h http.Header, status int) (string, bool) {
	values, _ := cacheHeaderValues(h, "Content-Encoding")
	current := strings.Trim(strings.Join(values, ","), " \t")
	if current != "" && !strings.EqualFold(current, "identity") {
		return "", n.prefs.acceptsStack(current)
	}
	_, partial := cacheHeaderValues(h, "Content-Range")
	if !n.prefs.present || status == http.StatusPartialContent || partial || !n.transform || !cacheControlAllows(h, "no-transform") {
		return "", n.prefs.weight("identity") > 0
	}
	return n.preferredCoding()
}

func (p encodingPreferences) acceptsStack(current string) bool {
	for coding := range strings.SplitSeq(current, ",") {
		coding = normalizeCoding(strings.Trim(coding, " \t"))
		if coding == "*" || !httpguts.ValidHeaderFieldName(coding) || p.weight(coding) == 0 {
			return false
		}
	}
	return true
}

func (n *compressionNegotiator) preferredCoding() (string, bool) {
	coding, quality := "", 0
	if n.brotli {
		coding, quality = "br", n.prefs.weight("br")
	}
	if q := n.prefs.weight(codingGzip); n.gzip && q > quality {
		coding, quality = codingGzip, q
	}
	if identity, explicit := n.prefs.quality["identity"]; explicit && identity > quality {
		return "", true
	}
	if quality > 0 {
		return coding, true
	}
	return "", n.prefs.weight("identity") > 0
}
