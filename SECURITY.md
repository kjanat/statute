# Security policy

statute is a reverse proxy. Vulnerabilities can affect every deployment that depends on it. Please do not file security issues publicly — that exposes users to the same vulnerability while we work on a fix.

## Reporting a vulnerability

Use GitHub's [private vulnerability reporting](https://github.com/kjanat/statute/security/advisories/new) on this repository. If that is not available, email `security@kajkowalski.nl` with:

- A description of the vulnerability and its impact.
- Steps to reproduce, or proof-of-concept code.
- The statute version (or commit SHA) you observed it on.
- Whether you intend to disclose publicly, and on what timeline.

We aim to acknowledge new reports within **3 working days** and to land a fix or mitigation within **14 days** for high-severity issues. After a fix lands, we will:

- Publish a `GHSA-` advisory describing the issue and the affected versions.
- Tag a patch release on the affected `vX.Y` branch.
- Credit you in the advisory unless you prefer anonymity.

## Supported versions

While statute is pre-1.0, only the latest minor release receives security fixes. After 1.0, this section will list specific supported version ranges.

| Version | Supported |
| ------- | --------- |
| 0.2.x   | ✅        |
| < 0.2   | ❌        |

## Hardening recommendations

These are not vulnerabilities in statute — they are deployment requirements that statute does not (and cannot) enforce. Operators are responsible for applying them.

- **`ReadHeaderTimeout`** must be set. The default scaffold uses `5s`. Without it, statute is vulnerable to Slowloris and Slow-Read attacks. The framework forces a `5s` default if you leave the field empty, but if you set it to `0` explicitly, the Slowloris protection is disabled.
- **`Storage` path for AutoTLS** must be persistent and on a private filesystem. The account key in there can issue arbitrary certificates for your domains.
- **Cloudflare API token** (DNS-01) must be scoped to the specific zones it manages. Anyone with the token can issue arbitrary certs for those zones.
- **Metrics listener** must be private (loopback or a private VLAN). It exposes pprof under `/debug/pprof/*` — debugging gold for an attacker.
- **`BehindCloudflare()`** must only be enabled when the listener is actually fronted by Cloudflare. Without that guarantee, `CF-Connecting-IP` is forgeable and rate limiting / IP-based controls collapse.
- **`BasicAuth`** users must be stored as bcrypt hashes, not plaintext. The framework rejects non-bcrypt values at resolve time.
- **`Defaults.WriteTimeout`** should accommodate the slowest legitimate response on the listener. The default is `30s`; lower it where appropriate.

## Out of scope

- Volunteer-effort security research on third-party libraries. Report those to the upstream project.
- Vulnerabilities in deprecated unsupported versions (see table above).
- Theoretical concerns without a concrete attack vector. Open a regular issue instead.
