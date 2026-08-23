# Running statute behind Cloudflare

This guide covers the operational details of deploying statute as an origin server behind a Cloudflare proxy. It covers what `BehindCloudflare()` does, when to choose HTTP-01 vs DNS-01, the Cloudflare-side settings each requires, and the failure modes you'll hit if any of them are wrong.

## TL;DR

```go
// HTTP-01 mode (no API key). Works for non-wildcard certs.
// Requires public :80 reachable.
statute.HTTPS(":443",
    statute.AutoTLS("example.com").Email("ops@example.com").Storage("/var/lib/statute/certs"),
    statute.HTTP2(),
    statute.BehindCloudflare(),
)
```

```go
// DNS-01 mode (API token required). Works for wildcards. Does not need :80.
statute.HTTPS(":443",
    statute.AutoTLS("*.example.com", "example.com").
        Email("ops@example.com").
        Storage("/var/lib/statute/certs").
        CloudflareDNS01(token).Zone(zoneID),
    statute.HTTP2(),
)
```

`BehindCloudflare()` is independent of the cert mode. Use it whenever Cloudflare proxies traffic to the origin, regardless of how certs are issued.

## Why Cloudflare requires special handling

Cloudflare's proxy ("orange-cloud" mode) terminates TLS at the edge and re-encrypts to the origin. This breaks two assumptions the default ACME and request-handling paths make.

The default `autocert.Manager` advertises `acme-tls/1` in ALPN so it can satisfy TLS-ALPN-01 challenges. Let's Encrypt validates by opening a TLS connection to the origin with that ALPN, expecting a self-signed cert with the challenge token in a SAN extension. Cloudflare does not pass through custom ALPN protocols — its edge handshakes the validator with its own cert and re-encrypts to the origin without forwarding the ALPN selection. The origin's TLS-ALPN-01 path never executes. Validation fails with no informative error.

For request handling: every request to the origin arrives via Cloudflare's IP ranges. `r.RemoteAddr` is therefore always a Cloudflare IP, not the real client. Rate limiting keyed on client IP buckets all clients into a few cells (one per CF edge node) and becomes useless. IP-hash load balancing collapses the same way. Access logs show the CF edge as the source.

## What `BehindCloudflare()` does

`BehindCloudflare()` is a listener option that fixes both. It does two things:

1. **Drops `acme-tls/1` from the listener's ALPN advertisement.** autocert's TLS config normally lists `["h2", "http/1.1", "acme-tls/1"]`. Behind Cloudflare we want `["h2", "http/1.1"]` so autocert does not try (and silently fail) TLS-ALPN-01 — provisioning falls back to HTTP-01 cleanly.
2. **Tags the request context.** A small middleware sets a context key on every request received on this listener. `clientIP()` reads it and returns `CF-Connecting-IP` (or `True-Client-IP` as a fallback) instead of `r.RemoteAddr` and `X-Forwarded-For`. CF-Connecting-IP is populated by Cloudflare's edge and is not user-controllable on the path through the proxy.

The option does not enforce that the request actually came from a Cloudflare IP range. Adding that would require keeping the CF IP list current; a stale list can DOS the deployment if CF rotates ranges, so we instead trust the network path the operator has configured.

**This means**: if the listener is reachable directly (not only via Cloudflare), `BehindCloudflare()` alone becomes a security hole. Any client can send a forged `CF-Connecting-IP` header and dictate the IP statute uses for rate limiting and access logging. For that shared-listener topology, keep `BehindCloudflare()` — it still owns the ACME behaviour above — and add a peer-scoped trust policy beside it:

```go
statute.HTTPS(":443",
    statute.BehindCloudflare(),
    statute.TrustedProxy(cfRanges...).ClientIPHeader("CF-Connecting-IP"),
)
```

`TrustedProxy` takes precedence over `BehindCloudflare()`'s blanket header trust wherever the client IP is resolved: the CF headers count only when the connection's direct peer is inside the declared Cloudflare ranges, and every other peer is attributed by its own address, forged headers ignored. The ranges are static CIDRs you maintain (Cloudflare publishes them at cloudflare.com/ips); statute deliberately does not fetch the list, since a stale auto-refreshed list can take a deployment down when ranges rotate.

## HTTP-01 vs DNS-01: when to use which

**Use HTTP-01 (the default) when**:

- You don't need wildcard certs
- Port 80 is reachable from the public internet
- You want the smallest possible attack surface (no API token to leak)

**Use DNS-01 when**:

- You need a wildcard cert (`*.example.com`). RFC 8555 §8.4 requires DNS-01 for wildcards.
- Port 80 is not reachable (private network, Cloudflare-only origin, firewall)
- You manage many subdomains and want a single cert covering them all

The DNS-01 path requires a Cloudflare API token with `Zone.DNS:Edit` on the zone(s) covering your domains. Generate one at <https://dash.cloudflare.com/profile/api-tokens> using the "Edit zone DNS" template, scoped to the specific zones — broader scopes only increase blast radius if the token leaks.

## Cloudflare-side settings for HTTP-01

Three Cloudflare-side settings matter:

### SSL/TLS mode: Full (Strict)

Set this in **SSL/TLS → Overview → SSL/TLS encryption mode**.

Anything weaker (Flexible, Full without Strict) accepts any cert at the origin, or none at all. With Full (Strict), Cloudflare requires the origin to present a publicly-trusted cert — which is exactly what autocert provisions. Full (Strict) and AutoTLS are symbiotic: autocert provisions the cert that Strict requires, and Strict prevents downgrade attacks.

### "Always Use HTTPS": disabled for the challenge path

The HTTP-01 challenge serves a token at `/.well-known/acme-challenge/<token>` over plain HTTP on port 80. If "Always Use HTTPS" is on, Cloudflare 301-redirects the request to HTTPS before it ever reaches the origin's port-80 handler. The Let's Encrypt validator does not follow that redirect to a TLS endpoint that doesn't yet have a valid cert — chicken and egg.

Add a Page Rule or Configuration Rule:

- Match: `URI Path` matches `/.well-known/acme-challenge/*`
- Setting: **Always Use HTTPS** = Off

Or disable "Always Use HTTPS" globally (in **SSL/TLS → Edge Certificates**) and use the redirect listener in your statute config to do the redirect itself. statute's redirect listener already excludes the ACME challenge path from the redirect when AutoTLS is configured — `autocert.HTTPHandler` wraps the redirect handler and serves the challenge before falling through.

### WAF skip rule for the challenge path

Default WAF rule sets occasionally flag the random-looking token URLs (long alphanumeric paths look anomalous). Add a Skip rule:

- Match: `URI Path` starts with `/.well-known/acme-challenge/`
- Skip: Managed Rules, Bot Fight Mode, Rate Limiting

The challenge path is unauthenticated and accessed by Let's Encrypt's validators, which legitimately rotate IP addresses. Security policies designed for human traffic mis-fire here.

## Cloudflare-side settings for DNS-01

DNS-01 needs essentially nothing on the Cloudflare side beyond the API token. The provisioning flow is:

1. statute calls Let's Encrypt to start a cert order
2. Let's Encrypt returns a DNS-01 challenge token per identifier
3. statute computes the TXT value and writes a `_acme-challenge.<host>` record via Cloudflare's API
4. statute waits 15s for propagation, then tells Let's Encrypt to validate
5. Let's Encrypt queries DNS, sees the TXT record, marks the challenge valid
6. statute finalises the order and persists the cert
7. statute deletes the TXT record (best-effort cleanup)

Cloudflare's authoritative DNS is fast (~10s propagation) so the 15-second wait is conservative. The library does not poll DNS itself before telling Let's Encrypt to validate — Let's Encrypt does its own retries.

If you have many zones in the account, pin the zone explicitly to skip auto-discovery:

```go
statute.AutoTLS("api.example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs").
    CloudflareDNS01(token).Zone("a1b2c3d4e5f6...")
```

The zone ID is shown on the zone overview page in the Cloudflare dashboard.

## Storage layout for DNS-01

DNS-01 mode persists state under `<storage>/dns01/`:

```
<storage>/
└── dns01/
    ├── account.key        # ACME account private key (ECDSA P-256, PEM)
    ├── example.com.crt    # full cert chain (PEM, leaf first)
    └── example.com.key    # cert private key (PEM, ECDSA P-256)
```

The directory must persist across restarts. The standard mistakes — wiping it on every container build, mounting it as `tmpfs`, or forgetting to back it up — all cause re-issuance on every restart and rate-limit lockout in production.

A renewal goroutine wakes hourly. Any cert whose leaf expires within 30 days is re-issued. Renewal failures are logged but do not crash the process; the previous valid cert continues serving until it actually expires.

## CF-Connecting-IP: which header to trust

When `BehindCloudflare()` is enabled, statute checks headers in this order:

1. `CF-Connecting-IP` — Cloudflare's primary header for the originating client's IP. Always present on requests via the CF proxy.
2. `True-Client-IP` — only present on Cloudflare Enterprise plans. Same value as `CF-Connecting-IP` when both are present.
3. `r.RemoteAddr` — the connecting peer (Cloudflare's edge node).

`X-Forwarded-For` is deliberately not in the list: without explicit trust configuration it is a client-controlled header, and consulting it would let a direct client dictate the address used for rate limiting, IP lists, and client-IP route matching. Forwarded headers count only under a `TrustedProxy` policy or the Cloudflare pair above.

This ordering means: on a real Cloudflare deployment, the rate limiter, IP-hash strategy, and access log all key on the real client IP. If statute receives a request that doesn't come via Cloudflare (someone discovered the origin and connected directly), the CF headers are absent and the code falls back to `r.RemoteAddr` — which is the attacker's real IP. So degraded behaviour is graceful, not insecure.

## Failure modes

**Validation hangs and eventually times out (HTTP-01)**: The most common cause is the "Always Use HTTPS" trap. The validator gets a 301 redirect from Cloudflare to HTTPS and refuses to follow it. Look for repeated `301` responses on `/.well-known/acme-challenge/` in the access log. Fix with a Page Rule or Configuration Rule.

**Validation fails with `403 Forbidden` from the validator's perspective (HTTP-01)**: A WAF rule is blocking the challenge path. Add a Skip rule.

**`cloudflare: no zone found for "foo.example.com"` (DNS-01)**: The API token does not have access to a zone covering the requested domain. Either the token's zone scope is wrong, or the token was issued in a different account than the one hosting the zone. The token must be scoped to "Specific zone" (the same zone covering your domain) or "All zones in account" (and the account must own the zone).

**DNS-01 succeeds initially, fails on renewal months later**: usually a token rotation that wasn't propagated to the deployment's environment, or the storage directory was wiped. ACME accounts and certs both live there; both are needed for renewal.

**Cert is issued but browsers show a different issuer than expected**: Cloudflare's "Authenticated Origin Pulls" (mTLS) and "Origin CA" (CF-issued cert for edge↔origin) generate certificates that browsers do not trust — only Cloudflare's edge does. statute's autocert path issues real Let's Encrypt certs that are publicly trusted; they are different products. You cannot mix them.

**Rate limiter buckets all clients together**: `BehindCloudflare()` is missing or `CF-Connecting-IP` is empty. Verify the listener actually has `BehindCloudflare()` and that requests are arriving via Cloudflare (not bypassing it via the origin's IP).

**`statute_requests_by_status_total{status="200"}` is high but `CF-Connecting-IP` is in the access log as `r.RemoteAddr`**: `BehindCloudflare()` is missing.

**HTTP/3 not working**: Cloudflare proxies HTTP/3 to clients but always re-encrypts to origin over HTTP/2 (or HTTP/1.1). The `HTTP3()` option on a behind-CF listener will start a UDP listener that never receives traffic from Cloudflare. The `Alt-Svc` header advertised to browsers is also stripped by Cloudflare. If Cloudflare is your client-facing layer, drop `HTTP3()` from the origin config — Cloudflare's edge speaks HTTP/3 to clients on your behalf.

## What `BehindCloudflare()` does NOT do

- It does not refresh the CF IP list. Add an external mechanism if you want to enforce that requests come from CF.
- It does not enable Authenticated Origin Pulls (mTLS between CF and origin). That requires StaticTLS with a CF-issued client CA, configured separately.
- It does not change the upstream-facing transport. Requests to your backends still go out as configured in the pool's `Transport`.

## Quick checklist

For HTTP-01 deployment behind Cloudflare:

- [ ] Cloudflare SSL/TLS mode is **Full (Strict)**
- [ ] Page Rule disables "Always Use HTTPS" for `/.well-known/acme-challenge/*` (or globally)
- [ ] WAF Skip rule for `/.well-known/acme-challenge/*`
- [ ] DNS A/AAAA records for the listener's domains point to Cloudflare (proxied)
- [ ] Origin :80 reachable from Cloudflare (no firewall blocking CF IP ranges)
- [ ] `BehindCloudflare()` on the HTTPS listener
- [ ] `Storage` directory persistent across restarts

For DNS-01 deployment behind Cloudflare:

- [ ] API token created with Zone.DNS:Edit, scoped to the relevant zone(s)
- [ ] Token wired in via environment variable (don't hardcode)
- [ ] `CloudflareDNS01(token)` on the AutoTLS chain
- [ ] `BehindCloudflare()` if CF actually proxies the traffic (still recommended for client IP attribution even with DNS-01)
- [ ] `Storage` directory persistent and backed up
