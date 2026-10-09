package statute

import (
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"statute.kjanat.dev/resolved"
)

// accessLogMiddleware writes a single JSON line per request to the configured
// destination after handler exit, including aborts. Panic values propagate
// to the server without being included in the access record.
//
// When SampleRate < 1.0, successful (2xx, 3xx) requests are sampled at the
// configured rate. Errors and aborted responses bypass sampling within the
// configured status filter.
func accessLogMiddleware(cfg resolved.AccessLog, next http.Handler) http.Handler {
	if !cfg.Enabled || cfg.Writer == nil {
		return next
	}
	enc := newSafeEncoder(cfg.Writer)
	rate := cfg.SampleRate
	if rate <= 0 {
		rate = 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: 200, multiplexed: r.ProtoMajor >= 2}
		// Direct composition below the RequestID middleware (as unit
		// fixtures do) finds the value already in the inbound context.
		inherited := requestIDFromContext(r.Context())
		// The holder lets the route-level RequestID middleware hand its
		// identifier back up to this listener-level wrapper.
		r, rid := installRIDHolder(r)
		defer func() {
			sample := rate
			if !ww.returned || ww.bodyError {
				sample = 1
			}
			if !shouldLog(ww.observedStatus(), sample, cfg.Statuses) {
				return
			}
			entry := accessLogEntry(r, ww, start)
			id := rid.load()
			if id == "" {
				id = inherited
			}
			if id != "" {
				entry["request_id"] = id
			}
			addVerifiedClientCert(entry, r)
			enc.Encode(entry)
		}()
		next.ServeHTTP(ww, r)
		ww.returned = true
	})
}

func accessLogEntry(r *http.Request, ww *statusRecorder, start time.Time) map[string]any {
	return map[string]any{
		"ts":            start.UTC().Format(time.RFC3339Nano),
		"method":        r.Method,
		"host":          r.Host,
		"path":          r.URL.Path,
		"query":         r.URL.RawQuery,
		"remote":        clientIP(r),
		"user_agent":    r.UserAgent(),
		"referer":       r.Referer(),
		"status":        ww.observedStatus(),
		"body_bytes":    ww.bytes,
		"aborted":       !ww.returned,
		"body_error":    ww.bodyError,
		"duration_us":   time.Since(start).Microseconds(),
		"proto":         r.Proto,
		"forwarded_for": r.Header.Get("X-Forwarded-For"),
	}
}

func addVerifiedClientCert(entry map[string]any, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return
	}
	cert := r.TLS.VerifiedChains[0][0]
	entry["client_cert_subject"] = cert.Subject.String()
	var sans []string
	for _, name := range cert.DNSNames {
		sans = append(sans, "dns:"+name)
	}
	for _, email := range cert.EmailAddresses {
		sans = append(sans, "email:"+email)
	}
	for _, ip := range cert.IPAddresses {
		sans = append(sans, "ip:"+ip.String())
	}
	for _, uri := range cert.URIs {
		sans = append(sans, "uri:"+uri.String())
	}
	if len(sans) > 0 {
		entry["client_cert_sans"] = sans
	}
}

// shouldLog returns true when this request should be written. The status
// filter is a hard gate ahead of every other rule: a status outside every
// range is never logged, even a 5xx. Within the allowed set, errors are
// always logged and successful requests pass at the configured sample rate.
func shouldLog(status int, rate float64, statuses []resolved.StatusRange) bool {
	if len(statuses) > 0 && !statusAllowed(status, statuses) {
		return false
	}
	if status >= 400 {
		return true
	}
	if rate >= 1 {
		return true
	}
	return rand.Float64() < rate //nolint:gosec // G404: log sampling is not security-sensitive; math/rand is intentional here
}

// statusAllowed reports whether status falls in one of the resolved ranges.
func statusAllowed(status int, statuses []resolved.StatusRange) bool {
	for _, r := range statuses {
		if status >= r.From && status <= r.To {
			return true
		}
	}
	return false
}

// safeEncoder serializes JSON writes so multiple goroutines do not interleave.
type safeEncoder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newSafeEncoder(w jsonWriter) *safeEncoder {
	return &safeEncoder{enc: json.NewEncoder(w)}
}

// Encode serialises v as one JSON line under a mutex so concurrent
// requests cannot interleave access-log entries.
func (s *safeEncoder) Encode(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(v) //nolint:errchkjson // best-effort access-log write; v is always the internal log entry and a failed line must not fail the request
}

// jsonWriter is satisfied by io.Writer; aliased for clarity at call site.
type jsonWriter interface {
	Write(p []byte) (n int, err error)
}

// clientIP resolves the address the request is attributed to — by the
// access log, rate limiting, the IP lists, IPHash, and ClientIPs route
// matching. Forwarded headers count only under explicit trust
// configuration: a listener-level TrustedProxy policy decides per peer, and
// BehindCloudflare trusts the Cloudflare pair listener-wide. Without
// either, the connecting peer is the client — an unconditional
// X-Forwarded-For fallback would let any client pick its own identity,
// which is route-selection and allow-list bypass, not attribution.
func clientIP(r *http.Request) string {
	// The TrustedProxy policy governs alone: it decides per peer whether
	// forwarded headers count, so the fallbacks below must not resurrect a
	// header the policy just refused.
	if p := trustedProxyFromContext(r); p != nil {
		return p.clientIP(r)
	}
	if isBehindCloudflare(r) {
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			return cf
		}
		if tc := r.Header.Get("True-Client-IP"); tc != "" {
			return tc
		}
	}
	if peer, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return peer.Addr().Unmap().String()
	}
	return r.RemoteAddr
}
