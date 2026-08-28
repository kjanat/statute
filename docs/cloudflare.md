# Running statute behind Cloudflare

This guide covers the operational details of deploying statute as an origin server behind a Cloudflare proxy. It covers what `BehindCloudflare()` does, when to choose HTTP-01 vs DNS-01, the Cloudflare-side settings each requires, and the failure modes you'll hit if any of them are wrong.

## TL;DR

```go
// HTTP-01 mode (no API key). Works for non-wildcard certs.
// Requires public :80 reachable and a plain HTTP listener in the config.
statute.HTTP(":80").RedirectTo("https"),
statute.HTTPS(":443",
    statute.AutoTLS("example.com").
        Email("ops@example.com").
        Storage("/var/lib/statute/certs").
        HTTP01(),
    statute.HTTP2(),
    statute.BehindCloudflare(),
)
```

`.HTTP01()` is the recommended pin behind Cloudflare: without it the source
takes the automatic policy, which attempts TLS-ALPN-01 first — a challenge
Cloudflare's edge can never deliver. See below.

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

1. **Drops `acme-tls/1` from the listener's ALPN advertisement.** autocert's TLS config normally lists `["h2", "http/1.1", "acme-tls/1"]`. Behind Cloudflare we want `["h2", "http/1.1"]`, because a validator that negotiates `acme-tls/1` against the origin never gets there — Cloudflare terminates the handshake at the edge.

   Dropping the protocol does not stop autocert from _attempting_ the challenge. `Manager.supportedChallengeTypes` hard-codes `tls-alpn-01` first (`http-01` is appended only once `HTTPHandler` has been installed, which statute does only when the config has a plain HTTP listener), and `verifyRFC` picks the first supported type on a freshly created order. Suppressing the ALPN entry therefore turns the TLS-ALPN-01 attempt into a _failure_ rather than a hang: one invalid authorization and one abandoned order per issuance, after which `verifyRFC` loops, opens a **new** order, and tries HTTP-01 — which only exists as a fallback if a plain HTTP listener is in the config.

   To skip the burned attempt entirely, pin the source with `.HTTP01()`. A pinned source issues through statute's in-tree ACME manager, which only ever attempts HTTP-01: no `acme-tls/1` advertisement, no failed validation, no extra order. Behind Cloudflare that is the recommended configuration; `BehindCloudflare()` still matters for client-IP attribution, and for any unpinned source on the listener.
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

**Use HTTP-01 (`.HTTP01()`) when**:

- You don't need wildcard certs
- Port 80 is reachable from the public internet
- You want the smallest possible attack surface (no API token to leak)

HTTP-01 is not the default policy: an `AutoTLS` source with neither `.HTTP01()` nor `CloudflareDNS01()` takes the automatic policy and attempts TLS-ALPN-01 first. `.HTTP01()` also makes the requirement explicit at resolve time — a pinned source without a plain HTTP listener anywhere in the config is a resolve error, not a silent per-start validation failure.

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
4. statute waits for propagation, then tells Let's Encrypt to validate
5. Let's Encrypt queries DNS, sees the TXT record, marks the challenge valid
6. statute finalises the order and persists the cert
7. statute deletes the TXT record (best-effort cleanup)

Step 4 is the one you can configure. By default it is a fixed 15-second sleep: Cloudflare's authoritative DNS is fast (~10s propagation), so the wait is conservative and Let's Encrypt's own retries cover the rest. `Propagation` replaces that default when 15 seconds is the wrong number.

## Propagation: waiting for the TXT record

`Propagation` takes a `statute.DNSPropagation` and applies to the one `AutoTLS` source it is called on. It has two halves, and a policy must use at least one:

```go
statute.AutoTLS("*.foo.example.test").
    Email("ops@example.test").
    Storage("/var/lib/statute/acme").
    CloudflareDNS01(token).
    Propagation(statute.DNSPropagation{
        Delay:     "30s",
        Resolvers: []string{"192.0.2.53:53", "198.51.100.53:53"},
    })
```

**Fixed delay.** `Delay` on its own is the default's shape with a duration you choose — sleep, then ask the CA to validate. Use it when you know your propagation time and don't want to name resolvers. Maximum 10 minutes.

**Resolver-verified polling.** `Resolvers` turns the wait into a check. After `Delay` (which may be omitted entirely, in which case polling starts immediately), statute queries each listed DNS server for `_acme-challenge.<host>` and does not ask the CA to validate until **every one of them** returns the expected TXT value. Each resolver is a `host:port` — the port is explicit, so `192.0.2.53:53`, not `192.0.2.53`, and an IPv6 host is bracketed, `[2001:db8::1]:53` — and a host may be an IP or a name. The resolved schema stores each address canonically (IP literals in canonical text form, hostnames lowercased, port in plain decimal), and one server listed twice in any spelling is a resolve error. A resolver that has served the value once is not queried again, and each round probes every still-pending resolver concurrently, one probe per resolver bounded by one `Interval` — so an unreachable resolver spends only its own probe budget, never the budget of the resolvers listed after it.

`Timeout` (default `"2m"`, maximum 10 minutes) is the deadline for that loop, measured from the end of `Delay`. `Interval` (default `"5s"`, clamped down to `Timeout` when the timeout is shorter; an explicit value must be at least 100ms and at most `Timeout`) is the cadence; the first round runs immediately rather than one interval later. Both govern polling alone, so setting either without `Resolvers` is a resolve error rather than a setting nothing reads. So are a negative or over-long delay, a zero or over-long timeout, an explicit interval below the floor or above the timeout, a resolver that is not a `host:port` with a port in 1–65535, a resolver listed twice in any spelling, a policy that waits for nothing (no positive delay and no resolvers — `Delay: "0s"` alone counts), and `Propagation` on a source with no `CloudflareDNS01` — no other challenge publishes a DNS record to wait for.

**Verification fails closed.** If the deadline passes with any resolver still not serving the value, issuance fails with an error naming the record and the laggards, and the CA is never asked to validate. That is the point: a failed check on our side costs nothing, while a failed CA validation spends one of the five validation failures Let's Encrypt allows per hostname per hour. The retry happens on the next handshake or renewal pass, after the usual cooldown.

Lookup errors during polling are not failures. A record that has not propagated yet is indistinguishable from `NXDOMAIN` or a `SERVFAIL` from a resolver still holding a negative answer, so an error only leaves that resolver unsatisfied for the round.

The whole propagation budget (`Delay` + `Timeout`) is added to the five-minute cap on one ACME order, so a long policy is not cancelled halfway through the wait it authorised.

The budget also shapes startup: DNS-01 certificates are issued synchronously before the listeners open, one domain after another, so a policy that ends up waiting its full `Delay` + `Timeout` delays the proxy's first accepted connection by that much per uncached domain. Size the policy for the zone, not for the worst imaginable case.

Which resolvers to list: the authoritative nameservers for the zone are the strongest signal, since that is what Let's Encrypt's validators ultimately query (Cloudflare shows them on the zone overview page). Public recursives such as `1.1.1.1:53` or `8.8.8.8:53` test a different thing — cache convergence — and can be slower to agree than the CA is.

The resolved schema and `-export` output carry the normalised policy under each DNS-01 source, defaults filled in and durations in nanoseconds.

If you have many zones in the account, pin the zone explicitly to skip auto-discovery:

```go
statute.AutoTLS("api.example.com").
    Email("ops@example.com").
    Storage("/var/lib/statute/certs").
    CloudflareDNS01(token).Zone("a1b2c3d4e5f6...")
```

The zone ID is shown on the zone overview page in the Cloudflare dashboard.

## Storage layout for the pinned challenges

Each pinned source persists state under `<storage>/<challenge>/` — `dns01/` for `CloudflareDNS01()`, `http01/` for `.HTTP01()`:

```text
<storage>/
├── dns01/
│   ├── account.key        # ACME account private key (ECDSA P-256, PEM)
│   ├── example.com.crt    # full cert chain (PEM, leaf first)
│   └── example.com.key    # cert private key (PEM, ECDSA P-256)
└── http01/
    ├── account.key        # a separate ACME account, registered on its own
    ├── api.example.com.crt
    └── api.example.com.key
```

Each subdirectory carries its own ACME account key, so a DNS-01 and an HTTP-01 source under one storage root register independently and may use different contact emails. Sources that do share a subdirectory share the account: statute rejects at resolve time a config whose sources disagree on `Email` there, because the second registration would return `ErrAccountAlreadyExists` and drop its contact.

The whole storage root must persist across restarts. The standard mistakes — wiping it on every container build, mounting it as `tmpfs`, or forgetting to back it up — all cause re-issuance on every restart and rate-limit lockout in production.

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
- It does not enable Authenticated Origin Pulls (mTLS between CF and origin). statute cannot verify client certificates today: no listener option requests, requires, or validates one. Authenticated Origin Pulls therefore has to be terminated in front of statute. Tracked in [#82](https://github.com/kjanat/statute/issues/82).
- It does not change the upstream-facing transport. Requests to your backends still go out as configured in the pool's `Transport`.

## Quick checklist

For HTTP-01 deployment behind Cloudflare:

- [ ] `AutoTLS(...).HTTP01()` on the source, and a plain HTTP listener in the config
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
- [ ] `Propagation(...)` if the default 15s wait is wrong for the zone — a longer `Delay`, or `Resolvers` to verify against before validating
- [ ] `BehindCloudflare()` if CF actually proxies the traffic (still recommended for client IP attribution even with DNS-01)
- [ ] `Storage` directory persistent and backed up
