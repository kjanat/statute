package statute

// Middleware is a marker interface for surface middleware values. Concrete
// middleware constructors return values that satisfy this interface.
type Middleware interface {
	statuteMiddleware()
}

// RateLimitKey selects what attribute of the request a rate limit is keyed on.
type RateLimitKey int

const (
	// ClientIP keys the rate limiter on the client's IP address.
	ClientIP RateLimitKey = iota
	// HostHeader keys the rate limiter on the Host header.
	HostHeader
)

// String returns the canonical name of the key.
func (k RateLimitKey) String() string {
	switch k {
	case ClientIP:
		return "client_ip"
	case HostHeader:
		return "host"
	default:
		return enumUnknown
	}
}

type rateLimitMW struct {
	rate string
	key  RateLimitKey
}

func (*rateLimitMW) statuteMiddleware() {}

// RateLimit returns a rate-limit middleware. The rate string is of the form
// "N/unit" where unit is one of s, min, h. For example "100/min".
func RateLimit(rate string) *rateLimitMW {
	return &rateLimitMW{rate: rate, key: ClientIP}
}

// Per sets the key the limiter buckets on. Defaults to ClientIP.
func (r *rateLimitMW) Per(k RateLimitKey) *rateLimitMW {
	r.key = k
	return r
}

type retryMW struct {
	max        int
	onStatuses []int
}

func (*retryMW) statuteMiddleware() {}

// RetryOption configures the Retry middleware.
type RetryOption interface {
	applyRetry(*retryMW)
}

type onStatusOpt struct{ codes []int }

func (o onStatusOpt) applyRetry(r *retryMW) { r.onStatuses = append(r.onStatuses, o.codes...) }

// OnStatus retries when the upstream returns any of the given status codes.
func OnStatus(codes ...int) RetryOption {
	return onStatusOpt{codes: append([]int(nil), codes...)}
}

// Retry returns a retry middleware with the given maximum attempts and options.
func Retry(max int, opts ...RetryOption) *retryMW {
	r := &retryMW{max: max}
	for _, o := range opts {
		o.applyRetry(r)
	}
	return r
}

type timeoutMW struct{ dur string }

func (*timeoutMW) statuteMiddleware() {}

// Timeout returns a per-request timeout middleware.
func Timeout(dur string) *timeoutMW { return &timeoutMW{dur: dur} }

type cacheMW struct{ ttl string }

func (*cacheMW) statuteMiddleware() {}

// Cache returns a response-cache middleware with the given TTL.
func Cache(ttl string) *cacheMW { return &cacheMW{ttl: ttl} }

// CompressAlgo identifies a content-encoding algorithm.
type CompressAlgo int

const (
	// Gzip compression.
	Gzip CompressAlgo = iota
	// Brotli compression.
	Brotli
)

// String returns the canonical name of the algorithm.
func (a CompressAlgo) String() string {
	switch a {
	case Gzip:
		return "gzip"
	case Brotli:
		return "br"
	default:
		return enumUnknown
	}
}

type compressMW struct{ algos []CompressAlgo }

func (*compressMW) statuteMiddleware() {}

// Compress returns a response-compression middleware that negotiates one of
// the listed algorithms based on the request's Accept-Encoding header.
func Compress(algos ...CompressAlgo) *compressMW {
	return &compressMW{algos: append([]CompressAlgo(nil), algos...)}
}

type etagMW struct{}

func (*etagMW) statuteMiddleware() {}

// ETag returns a middleware that adds ETag headers to static file responses
// and serves 304 Not Modified for matching If-None-Match requests.
func ETag() *etagMW { return &etagMW{} }
