# Traefik migration gaps

Implementation contract for [#259](https://github.com/kjanat/statute/issues/259).
Behavioral comparisons use the Traefik version in `traefikoracle/go.mod`.

- **Owner:** `internal/migrate` translates inputs. Listener ingress owns header
  sanitation, alias handling and URL normalization. Middleware remains route
  policy; transport and health remain pool policy.
- **Invariants:** existing listener defaults stay unchanged. Generated programs
  refuse startup with unresolved findings. Audits remain offline and do not read
  credentials or evaluate environment bindings.
- **Boundaries:** surface policy resolves into `resolved.Listener`; runtime,
  export, graph and lint consume that contract. HTTP and HTTPS share ingress
  processing. Existing listener proxy trust remains the client-identity authority.
- **Failure:** invalid policies fail resolution. Rejected alias headers return
  400 before routing. Unsupported source policies block generation with their
  concrete semantic difference; dependent routes identify the root finding.
- **Lifecycle:** ingress has immutable compiled policy and per-request work.
  It adds no background worker, listener, provider or storage owner.
- **Interactions:** sanitation precedes route middleware, ACLs and rate limiting.
  Normalized paths reach static, Docker and fallback dispatch once, before path
  rewrites or Retry. Access logs keep the original path and see sanitized headers;
  ingress rejection participates in request metrics.
- **Acceptance:** direct/trusted peer tests, spoofed canonical and alias headers,
  encoded paths and queries, redirect captures and method statuses, middleware
  order, shared pools, provider-qualified dependencies and overlapping routes.
  Synthetic generated programs must compile, pass canonical checks and serve.

Add opt-in `IngressPolicy` through listener options, including plain HTTP via
`Listener.With`. The generator explicitly selects normalization and the source
alias strategy. Source `sanitizePath` remains a separate explicit setting.

Response-header operations may opt into HTTPS-only emission. Redirect actions
may declare a separate non-GET status. These choices stay on their existing
route/listener owner and preserve defaults for existing declarations.

Translate supported security headers, rate limits and redirect subsets. Keep
reviewable candidates for buffering, compression, authentication and rewrites;
report resource, ordering and protocol differences individually. Preserve source
values through explicit environment bindings where required by the audit.

TLS, ACME and health features without equivalent native policy remain explicit
decisions. Do not promote host policy to an entire listener, remove health/TLS
policy, or silently substitute a cross-provider service.
