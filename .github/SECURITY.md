# Security policy

statute is a reverse proxy, so a bug here can hit every deployment. Don't file security issues publicly.

## Reporting

Use GitHub's [private vulnerability reporting](https://github.com/kjanat/statute/security/advisories/new), or email `security@kajkowalski.nl`. Include what it is, how to reproduce it, and the version/commit.

I'll look at it when I can. No promised response time, no promised turnaround — don't expect a quick fix. If you want credit for the find, say so and I'll add it somewhere.

## Supported versions

Pre-1.0: only the latest minor gets fixes.

| Version | Supported |
| ------- | --------- |
| 0.6.x   | ✅        |
| < 0.6   | ❌        |

## Hardening recommendations

Not bugs in statute — deployment requirements operators must apply themselves.

- **`ReadHeaderTimeout`** defaults to `5s`; keep it nonzero. Setting it to `0` disables Slowloris protection.
- **`Storage` path for AutoTLS** must be persistent and on a private filesystem — the account key can issue arbitrary certs for your domains.
- **Cloudflare API token** (DNS-01) must be scoped to the zones it manages.
- **Metrics listener** must be private — it exposes pprof under `/debug/pprof/*`.
- **`BehindCloudflare()`** only when the listener is actually fronted by Cloudflare, else `CF-Connecting-IP` is forgeable.
- **`BasicAuth`** users must be bcrypt hashes, not plaintext.
- **`Defaults.WriteTimeout`** should fit the slowest legitimate response (default `30s`).

## Don't bother

- Bugs in someone else's library — not my code, not my problem. Go tell them.
- Anything in an old unsupported version — it's dead, it stays dead.
- "This could theoretically be bad" with no actual exploit — that's a regular issue, not a security one.
