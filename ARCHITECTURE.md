# Statute architecture

This document records the stable ownership boundaries and runtime invariants that
changes must preserve. It is intentionally about architecture, not a tour of every
public API.

When an issue requires changing one of these boundaries, make that an explicit
design decision before implementation. Do not let a convenient field placement
quietly become the new architecture.

## Configuration pipeline

Statute has a two-stage configuration model:

```text
surface config / builders
        ↓ Resolve
resolved.Config (canonical normalized model)
        ↓ compile
runtime handlers, listeners, pools, providers
```

`Resolve` owns parsing, normalization, defaults, and configuration validation.
The `resolved` package is the canonical machine-readable contract used by runtime
construction and by tooling such as export, graph, and lint. A feature is not
complete when only the surface or runtime understands it.

## Ownership model

| Layer          | Owns                                                                                                              | Must not absorb                                   |
| -------------- | ----------------------------------------------------------------------------------------------------------------- | ------------------------------------------------- |
| Route          | host/path/client matchers, one route action, route middleware                                                     | backend transport or state shared by other routes |
| Upstream pool  | backends, balancing strategy, backend health, transport, upstream Host/TLS policy                                 | router-specific middleware or matchers            |
| Listener       | ingress protocol, downstream TLS policy/material selection, trusted-proxy policy, listener wrapping/observability | route-specific policy                             |
| Docker router  | router rule expansion and router-scoped middleware references                                                     | service-wide backend state                        |
| Docker service | discovered backend/service attributes used to construct a pool                                                    | unioned router policy                             |
| Resolved model | normalized immutable configuration contract                                                                       | runtime-only mutable state                        |

A single pool may be shared by many static or Docker-derived routes. That sharing is
intentional. Therefore any behavior that can legitimately differ between two routes
using the same pool cannot be implemented by storing it only on the pool.

## Routing

Static routes compile in declaration order and are consulted before Docker's
current dynamic generation. Dynamic discovery must not shadow compiled static
configuration.

A compiled route combines:

- the resolved matcher/action,
- its ready-to-serve handler chain,
- parsed runtime-only matcher state such as client prefixes.

Route matching observes the incoming request before route path rewrites are applied.
A route action is mutually exclusive with the other route actions. When adding a
new action, update the complete current action set rather than copying an older
binary/three-way model from a stale plan.

## Middleware

Middleware declaration order is semantic: the first declared middleware is the
outermost ordinary wrapper.

Request-header operations and path rewrites are special. They are hoisted to the
route edge and applied once so downstream re-entry, especially `Retry`, cannot
repeat them per attempt. Consequences that changes must preserve:

- route matching sees the original path;
- the access log currently observes the original request path;
- downstream middleware, cache keys, and upstream proxying see the rewritten URL;
- a retried downstream handler sees one already-normalized request view, not an
  accumulating transformation;
- request cloning must include a distinct URL object when rewriting it.

Do not move a transform into the ordinary wrapper chain without re-evaluating every
re-entry path.

## Docker discovery

Docker labels are external input. Statute keeps the trust boundary in code:
container labels may select supported behavior, but they do not define arbitrary
middleware implementations.

Traefik-compatible `routers.<r>.middlewares` references are router-scoped. They
ride the matcher/routes derived from that router, preserve label order, and are
part of route identity. Routers sharing one service still keep distinct policy
while sharing the service's pool.

A referenced code-owned middleware name that is unavailable fails closed for the
affected router's routes. Sibling routers/services continue. Do not degrade a
requested auth/security policy into an unprotected route.

Dynamic generations are replaced atomically. A generation owns the dynamic pool
handlers it constructed; retiring a generation must not let a later generation
adopt already-shut-down runtime state merely because configuration fingerprints
match.

## Upstream pools and health

A pool owns backend selection and the transport shared by proxy traffic to its
backends. Active health probes use that same transport so backend TLS verification
cannot drift between health traffic and real traffic.

`UpstreamHost` is pool policy and applies consistently to proxied requests and,
where meaningful, active probes. Any future probe-specific Host override must
define precedence explicitly instead of silently creating two competing policies.

Health is backend state. When passive health is added or changed, define whether
failures are consecutive or windowed, whether Retry attempts count per backend
attempt or only by final client-visible outcome, how recovery happens, and how the
existing degraded-mode behavior interacts with demotion.

## Retry and re-entry

`Retry` may serve the downstream handler more than once for one client request.
Anything below it can run per attempt. Anything that must happen exactly once must
sit outside that re-entry boundary or carry explicit idempotence.

When a feature hooks reverse-proxy outcomes, distinguish backend-attempt state from
final request state. A request may fail on one backend, retry, and succeed on a
different backend.

## TLS and ACME

A listener may have multiple TLS sources. Certificate selection is routed by SNI:
exact match, then supported wildcard match, then hostless fallback. Once a source is
selected for an SNI name, an error from that source is not permission to silently
fall through to another policy.

Automatic challenge selection uses the shared autocert manager. Pinned HTTP-01 and
DNS-01 sources use in-tree managers. Their lifecycle and storage are distinct; do
not merge their state merely because they issue certificates for the same listener.

HTTP-01 work depends on a serving plain-HTTP challenge path. DNS-01 does not. Warm-up,
startup rollback, and shutdown ordering must preserve that distinction.

## Lifecycle

Lifecycle changes must state ownership rather than relying on `Serve` goroutines to
hide it.

For every resource introduced or moved into `Start`, identify:

1. construction ownership,
2. the point at which the OS/runtime resource is acquired,
3. failed-start cleanup,
4. normal `Shutdown` cleanup,
5. any goroutine that must be cancelled and awaited,
6. whether a failed `Start` is retryable and what state must be reset for that to
   be true.

TCP listeners, UDP packet connections, `http.Server` / HTTP/3 server objects, ACME
manager state, Docker reconciliation, and dynamic pool handlers are different
resources. Closing an owned socket and permanently closing a reusable server control
object are not interchangeable operations.

If a PR claims transactional or retryable startup, tests must prove both resource
release after failure and successful serving after retry. A nil return from the
second `Start` is insufficient.

## Observability

Listener-level observability wraps the routed content path. Access logging and
metrics use the final response status, including handlers that emit informational
1xx responses before the final status.

Response-writer wrappers must preserve interfaces needed by streaming and efficient
copy paths, such as flushing and `io.ReaderFrom`, when the underlying writer
supports them.

Status filtering, sampling, and "always log errors" rules are separate policies.
When adding a filter, define their precedence explicitly; do not let an old general
rule silently override an explicit user filter.

## Stable review questions

Before merging an architectural change, be able to answer all of these:

- Why is this state stored on this layer rather than the layer above/below it?
- What happens when two routes share the same pool but differ in this policy?
- What happens when an external reference is invalid?
- What runs once and what may run once per retry attempt?
- Which request view does routing, logging, caching, and the upstream observe?
- Which component owns cleanup after partial startup and normal shutdown?
- Do proxy traffic and health traffic still share the policies they are supposed to?
- Does the resolved/exported model describe exactly what runtime executes?
