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
	max             int
	onStatuses      []int
	maxResponseBody string
	bufferBudget    string
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
func Retry(maxAttempts int, opts ...RetryOption) *retryMW {
	r := &retryMW{max: maxAttempts}
	for _, o := range opts {
		o.applyRetry(r)
	}
	return r
}

// MaxResponseBody sets the maximum body retained per retry attempt. The default
// is 8 MiB. Overflow ends this Retry's attempt loop with an empty 502 response.
func (m *retryMW) MaxResponseBody(size string) *retryMW {
	m.maxResponseBody = size
	return m
}

// BufferBudget bounds response-body allocations shared by this Retry instance.
// The default is 64 MiB. Unavailable capacity produces an empty 503 response.
func (m *retryMW) BufferBudget(size string) *retryMW {
	m.bufferBudget = size
	return m
}

type timeoutMW struct{ dur string }

func (*timeoutMW) statuteMiddleware() {}

// Timeout returns a per-request timeout middleware.
func Timeout(dur string) *timeoutMW { return &timeoutMW{dur: dur} }

type cacheMW struct{ ttl string }

func (*cacheMW) statuteMiddleware() {}

// Cache returns a response-cache middleware with the given TTL.
// Requests with bodies or trailers bypass caching. HTTP/3 bypasses caching
// because late unannounced trailers cannot be excluded before lookup.
// Requests carrying Authorization or Cookie bypass lookup and storage. Private
// or Set-Cookie responses are not stored, including projected response headers.
// Routes whose RequestID writes Authorization or Cookie bypass Cache entirely.
// TLS client certificates also bypass lookup and storage, including unverified
// presented certificates. HTTPS without client certificates remains cacheable.
// Response no-cache also prevents storage; Cache does not revalidate entries.
// AllowIPs and DenyIPs must precede every enabled Cache; unsafe ordering fails
// configuration validation, including assembled Docker middleware chains.
// RequestID must also precede enabled caches. RateLimit outside Cache counts
// every request; RateLimit inside Cache counts only misses.
// Request or response Cache-Control: no-store prevents storage. Response-header
// operations can additionally prohibit storage, but cannot erase an upstream
// no-store prohibition. Request no-store does not invalidate existing entries.
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
// Explicit exclusions are respected; unavailable representations produce 406.
func Compress(algos ...CompressAlgo) *compressMW {
	return &compressMW{algos: append([]CompressAlgo(nil), algos...)}
}

type etagMW struct{ maxResponseBody, bufferBudget string }

func (*etagMW) statuteMiddleware() {}

// ETag buffers and hashes GET representations. HEAD renders the inner GET path
// to compute the same validator, then omits its body. Preconditions use that
// representation; middleware order determines whether hashing sees encoding.
func ETag() *etagMW { return &etagMW{} }

// MaxResponseBody sets the maximum body retained by the ETag render, including
// an internal GET for HEAD. The default is 8 MiB; overflow returns an empty 502.
func (m *etagMW) MaxResponseBody(size string) *etagMW {
	m.maxResponseBody = size
	return m
}

// BufferBudget bounds response-body allocations shared by this ETag instance.
// The default is 64 MiB. Unavailable capacity produces an empty 503 response.
func (m *etagMW) BufferBudget(size string) *etagMW {
	m.bufferBudget = size
	return m
}
