package statute

// Protocol and scheme identifier constants. Centralised because they appear
// in many places (listener scheme switching, ALPN advertisement, request
// matching) and a typo in a string literal would silently mis-route.
const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"

	alpnHTTP1   = "http/1.1"
	alpnHTTP2   = "h2"
	alpnHTTP3   = "h3"
	alpnACMETLS = "acme-tls/1"

	// enumUnknown is the conventional String() fallback for enum types whose
	// numeric value falls outside the declared constants.
	enumUnknown = "unknown"

	// accessLogFormatJSON is the only access-log format currently supported.
	accessLogFormatJSON = "json"

	// labelValueTrue is the canonical truthy docker label value; labels
	// are strings, so booleans arrive spelled out.
	labelValueTrue = "true"
)
