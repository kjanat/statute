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

| Layer           | Owns                                                                                                                  | Must not absorb                                         |
| --------------- | --------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------- |
| Route           | host/path/client matchers, one route action, route middleware                                                         | backend transport or state shared by other routes       |
| Upstream pool   | backends, balancing strategy, backend health, transport, upstream Host/TLS policy                                     | router-specific middleware or matchers                  |
| Listener        | ingress protocol, downstream TLS/client-auth policy, material selection, trusted-proxy policy, wrapping/observability | route-specific policy                                   |
| Docker router   | router rule expansion and router-scoped middleware references                                                         | service-wide backend state                              |
| Docker service  | discovered backends, strategy, and routes; exact-key code-owned pool policy                                           | router policy or another service's pool policy          |
| Docker workload | activation, readiness, and idle lifecycle of one discovered service                                                   | backend health semantics or another service's lifecycle |
| Resolved model  | normalized immutable configuration contract                                                                           | runtime-only mutable state                              |

A single pool may be shared by many static or Docker-derived routes. That sharing is
intentional. Therefore any behavior that can legitimately differ between two routes
using the same pool cannot be implemented by storing it only on the pool.

## Routing

Static routes compile in declaration order and are consulted before Docker's
current dynamic generation. Dynamic discovery must not shadow compiled static
configuration.

A configured `Fallback` handler is the router's terminal stage, reached only
after both tables and the current generation's Docker tombstones miss; unset, the
terminal behavior stays `http.NotFound`. It is not a route: it has no matcher and
no route middleware. It lives inside the content router, so everything wrapping the router keeps its precedence over it:
pending HTTP-01 challenge responses on a plain HTTP listener, Alt-Svc, and
listener observability all sit outside it, and a redirect-only listener never
reaches it. What each ACME source claims differs: an automatic source absorbs
the whole challenge namespace, while a pinned HTTP-01 source answers only its
pending tokens and passes other paths through to the router, where they are
routed normally: a static or Docker route may match one, and the fallback is
reached only when both tables and the tombstones miss.

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

`Docker().PoolPolicy(name, ...)` is the pool-scoped counterpart. Its exact key is
the resolved discovered-service identity (`foo` for native labels,
`foo@traefik` for a Traefik service). Docker owns the changing backends, strategy,
and routes; code owns the matched service's transport, upstream Host, active-health
configuration, and passive-health policy. A registered policy is authoritative for
all four fields, including their zero values. It is applied before the generation
fingerprint is computed, so an effective policy change replaces the pool handler
while an identical one preserves its connections and health state. A key matching
no discovered service produces a deduplicated provider warning. Policy never
crosses service identities or becomes router
middleware.

A discarded registration leaves a **tombstone**: a matcher carrying no upstream,
no middleware, and one fixed 404 refusal. Dispatch is static routes, then valid
Docker routes, then tombstones, then `Config.Fallback`. Tombstones exist because
a dropped registration used to end in the terminal 404; with a fallback
configured it would instead fall through into operator code that does not know
the registration asked for a policy statute could not supply. A registration is
a Traefik router or a container's native `statute.*` labels: both declare routes,
and discarding either has the same consequence.

The obligation is an envelope: for every rejected registration R and its
tombstone set T, every request Traefik would have matched must be matched by
some element of T. Refusing more than R claimed is allowed; refusing less is
not. Traefik and native statute labels compile to one matcher IR
(`HostKind` / `PathKind`); one dispatcher compares it. Traefik `PathPrefix`
is a byte prefix (`strings.HasPrefix`): `PathPrefix(`/admin`)` matches
`/admin-secret`, and both a serving Docker route and its tombstone use that
matcher. Statute `Match("/admin/*")` stays segment-aware. Traefik `Host()`
keeps the configured spelling and folds one trailing FQDN dot on the rule or
the request, so ``Host(`example.com.`)`` matches `example.com..`. Static
routes and native `statute.*` label routes keep statute's exact host
spelling. `Host("*")` / `Host("*.example.com")` and `Path()`/`PathPrefix()`
arguments with `%`, placeholders, or regexp syntax are rejected and
tombstoned: accepting them as literals would under-match and reach
`Config.Fallback`.
Derivation therefore widens: an unrepresentable conjunct is dropped, a disjunction
is a branch-aware union so one unreadable branch widens the whole rule, a negation
node becomes unconstrained in place, and a rule that cannot be bounded at all
becomes the global any-host `/*` tombstone. There is no unbounded drop that
refuses nothing.

This needs a second, tolerant reading of the rule beside `ParseRule`: the strict
lexer and parser abandon the sibling constraints that were the only thing
bounding the rule. The separate test-only `traefikoracle` module drives
Traefik's own parser and HTTP muxer across accepted and rejected boundary cases.
Inside statute, the two readers are held together by a differential fuzz
property: over every rule `ParseRule` accepts, the envelope must contain every
request its matchers match.

A router with no rule declares no match condition, so its request set is empty
and it leaves no tombstone; neither does a container that opted out explicitly,
nor one registered only by `ExposedByDefault` and carrying no `statute.*` label
of its own, whose any-host `/*` route is statute's inference. An `enable` label
that cannot be parsed is not an opt-out: the intent could not be read, the
routes vanish exactly as a rejection discards them, and the registration leaves
the envelope its other labels declared. A container that opted in with
`statute.enable` and named neither host nor path does leave one: it compiles to
that same any-host `/*` route, so it terminates every request it is given, and
dropping it in silence would be the widest under-refusal the tier can have.
TCP/UDP routers are out of the tier's domain: it expresses HTTP refusals only.

Tombstones belong to the generation that derived them and are replaced with it
atomically, so a router that becomes valid loses its tombstone in the same swap.
Absorption runs across the whole generation, so a global envelope leaves exactly
one tombstone: an operational event the provider logs, disabling the fallback
for every request in that generation. That announcement is keyed to the previous
generation's refusal: a rule that is repaired and later regresses must not
disable the fallback in silence.

Dynamic generations are replaced atomically. A generation owns the dynamic pool
handlers it constructed; retiring a generation must not let a later generation
adopt already-shut-down runtime state merely because configuration fingerprints
match.

## On-demand Docker workloads

Statute may activate a stopped Docker workload when a routed request needs it and
stop it again after an idle period. The scope is narrow on purpose:

- one Docker host, the one Statute already observes;
- activation driven only by routing demand Statute observes directly;
- no placement, scheduling, replicas, leader election, cluster membership, or
  storage provisioning;
- no jobs, deployment pipelines, canaries, or image promotion.

Routing remains the primary concern and lifecycle exists to make a routed service
available. A requirement that needs any of the excluded capabilities belongs
outside this layer.

Workload lifecycle is separate from backend health. Backend health begins healthy
and demotes on evidence of failure, and degraded mode still routes to primaries
when every backend is demoted. An activated workload has the opposite default: it
is unavailable until readiness is positively established, and requests wait before
any backend is eligible. A dormant or starting workload is therefore not an
unhealthy backend, and `backendState` carries no lifecycle meaning.

A dormant route still matches. Discovery keeps the workload identity, labels, and
route declaration of a stopped container, apart from whether a backend is usable
right now. Such a route is a real match: it does not become a route miss, does not
fall through to the next dispatch tier, and does not reach `Config.Fallback`.

Docker reporting a container as running is not readiness. An activated workload
serves no traffic until a readiness signal establishes it. Active-health semantics
begin from healthy and do not carry over.

Activation is single-flight. Concurrent requests for one dormant workload produce
one start operation, one readiness wait, and one outcome delivered consistently to
every waiter. Cancellation is explicit: one client disconnecting does not cancel an
activation the remaining waiters still need.

Activation failure is terminal for the request. A timeout or failure answers the
client, `503` with `Retry-After` where meaningful, and does not continue into
`Config.Fallback`. Operator code that never asked for the workload cannot answer
for it. The original request survives until proxying begins.

Idle is measured from request completion. An in-flight HTTP request, an open
WebSocket, and an open streaming response each hold the workload active, and the
idle timer starts when the last of them finishes. A request arriving while the
workload is stopping has one defined outcome and never proxies into a container
being torn down.

Lifecycle state belongs to the generation that owns it. Docker generations are
replaced atomically, and a retired generation may not mutate or cancel its
successor's state. That holds while a workload is starting, while labels change
during activation, while Statute shuts down mid-activation, and when Docker
reports a stop from outside.

Authority is code-owned. A container label may select or parameterize an activation
policy the binary already grants. A label alone never grants Statute authority to
start or stop a workload, following the trust boundary that governs code-owned
middleware and `PoolPolicy`.

## Upstream pools and health

A pool owns backend selection and the transport shared by proxy traffic to its
backends. Active health probes use that same transport so backend TLS verification
cannot drift between health traffic and real traffic.

For Docker-discovered pools, Docker supplies backends and strategy while an
exact-key `PoolPolicy` supplies the code-owned transport and health settings. The
policy reaches the same pool construction path as static configuration; it does not
create a second transport or health implementation.

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

Client-certificate authentication is listener-owned. One normalized `ClientAuth`
policy covers every certificate source and both the TCP and QUIC TLS configs built
for that listener. Route selection receives only connections admitted by the
handshake, and client-certificate identity has no matcher. Resolve validates only
shape and paths; construction loads the CA bundles before sockets open, and missing
or malformed material aborts the listener. A verified peer's subject and SANs may
enter the access log; certificates lacking a verified chain are omitted.

An ACME TLS-ALPN-01 validator presents no client certificate. A client-auth mode
that requires one therefore suppresses the challenge ALPN and needs a plain HTTP
listener when an automatic source is present, allowing autocert's HTTP-01 fallback.
Pinned HTTP-01 and DNS-01 sources keep their existing challenge ownership.

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

Access logs may describe a verified TLS client certificate from the request's
connection state. Enforcement remains in the TLS handshake.

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
- Which state is workload lifecycle and which is backend health?
- Does the resolved/exported model describe exactly what runtime executes?
