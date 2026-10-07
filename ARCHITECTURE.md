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

| Layer           | Owns                                                                                                                  | Must not absorb                                                             |
| --------------- | --------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| Route           | host/path/client matchers, one route action, route middleware                                                         | backend transport or state shared by other routes                           |
| Upstream pool   | backends, balancing strategy, backend health, transport, upstream Host/TLS policy                                     | router-specific middleware or matchers                                      |
| Listener        | ingress protocol, downstream TLS/client-auth policy, material selection, trusted-proxy policy, wrapping/observability | route-specific policy                                                       |
| Docker router   | router rule expansion and router-scoped middleware references                                                         | service-wide backend state                                                  |
| Docker service  | discovered backends, strategy, and routes; exact-key code-owned pool policy                                           | router policy or another service's pool policy                              |
| Docker workload | activation, readiness, and idle lifecycle of one container beneath a discovered service                               | backend health semantics, pool-wide state, or another container's lifecycle |
| Resolved model  | normalized immutable configuration contract                                                                           | runtime-only mutable state                                                  |

A single pool may be shared by many static or Docker-derived routes. That sharing is
intentional. Therefore any behavior that can legitimately differ between two routes
using the same pool cannot be implemented by storing it only on the pool.

## Routing

Static routes compile in declaration order and are consulted before Docker's
current dynamic generation. Dynamic discovery must not shadow compiled static
configuration.

`Hosts(...)` is surface declaration shorthand. Resolve expands its ordered host
list into consecutive single-host routes before the next declaration. Each route
retains the same action, client constraints, and middleware declaration, with its
own resolved middleware values and runtime middleware state. Upstream pools and
application handlers remain shared as with explicitly repeated `Host(...)` routes.
Host spelling and matching semantics are unchanged; export, graph, and lint see
the expanded routes. Mixing `Host` and `Hosts`, an empty host list or entry, and
case-insensitive duplicate entries fail resolution.

`FallbackRoutes` is an optional, declaration-ordered native route table at the
existing terminal no-match boundary. Dispatch is ordinary static routes, unchanged
Docker dispatch (including mutation quarantines and tombstones), terminal routes,
then the existing `Fallback` handler or `http.NotFound`. A matched response,
including a denial, proxy failure, or 404, never continues to another stage.
Backend failover and error interception remain outside terminal route selection.

Terminal routes use the same matcher, action, middleware resolver, and compiler
as ordinary routes. They reference only declared `Config.Upstreams`, never
ephemeral Docker pools. Both tables share each named pool's transport, health,
selection, and lifecycle owner; middleware state remains route-owned. A
fallback-only pool acquires and releases resources through the ordinary startup,
rollback, and shutdown path. The resolved model and export retain a separate
`FallbackRoutes` collection, with `fallback_routes[i]` diagnostic paths;
`HasFallback` continues to describe only the application handler.

The `Fallback` handler itself is not a route: it has no matcher and no route
middleware. Both terminal stages live inside the content router, so everything
wrapping the router keeps its precedence over them:
pending HTTP-01 challenge responses on a plain HTTP listener, Alt-Svc, and
listener observability all sit outside the router, and a redirect-only listener
never reaches it. What each ACME source claims differs: an automatic source absorbs
the whole challenge namespace, while a pinned HTTP-01 source answers only its
pending tokens and passes other paths through to the router, where they are
routed normally: a static or Docker route may match one, and terminal routing is
reached only when those tables and the generation's refusals miss.

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

AllowIPs and DenyIPs must precede every enabled Cache in their route. Resolution
rejects the opposite order because a cache hit would bypass the IP policy; no
wrapper is silently moved. Validation also runs on the final assembled Docker
chain, where a conflict refuses only that router through the existing tombstone
path. A nonpositive cache TTL disables the cache and creates no ordering conflict.
RequestID also precedes every enabled Cache: identity/header mapping and request-ID
publication run before lookup on every request. RateLimit retains declaration-order
semantics: outside Cache it counts requests, inside Cache it counts misses.

Each assembled route permits at most one RequestID middleware, regardless of
its configured input or output header. Resolve rejects duplicates in static and
fallback routes and individual Docker chains. Docker checks the combined default,
named, and label-derived chain again before publication; conflicting routers use
the existing refusal envelope without invalidating sibling routes or shared pools.
Retry outside RequestID may still invoke that single middleware per attempt.

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

Response-cache admission belongs to each route's Cache instance. Request or
downstream response `Cache-Control: no-store` prohibits new storage. Cache also
projects the route's hoisted response-header operations onto a copy before
admission, so route-added no-store is effective even though actual header
operations run only at final commitment. A projection cannot erase a downstream
storage prohibition. Retry re-entry shares the immutable route operations;
neither projection nor cache hits apply them to the actual response early.
No-store skips storage while preserving delivery and the route's failure policy.

Cache is shared between clients of its route. Authorization or Cookie field
presence in the effective request bypasses both lookup and storage. Origin or
projected response private directives (including qualified forms) and Set-Cookie
fields prohibit storage; raw response-header operations cannot erase an origin
privacy restriction. Public anonymous entries remain independently reusable.
Requests carrying TLS peer certificates or verified client chains also bypass
lookup and storage: certificate identity is not part of the shared key. This
includes optional and application-verified client certificates. Ordinary HTTPS
requests without client certificates remain eligible.
Origin or projected response no-cache also prohibits storage, including qualified
forms. Cache has no revalidation path; delivery remains unchanged and a later
response without that restriction can populate the route's cache.
If RequestID is configured to write Authorization or Cookie, assembly bypasses
every Cache in that route regardless of order: inner request clones cannot hide
credential creation from an outer cache. Other middleware retain their order.

Cache delegates conditional, range, request no-cache/no-transform, and malformed
cache-policy requests to the downstream producer without looking up or replacing
entries. Stored variants match the request values selected by the union of origin
and projected route Vary fields. Wildcard/invalid Vary and partial representations
cannot be stored. An entry owns its copied request-key values and expiry; changing
the Vary schema replaces incompatible variants. Compression adds Accept-Encoding
variance at commitment even when it negotiates identity, so an outer cache sees
the same selection dimensions for every encoding.

CORS contributes mandatory Origin variance to its route's response-header
projection and final commitment, after raw header operations. Every Cache on that
route sees the same selection dimension regardless of CORS placement or intervening
Retry/ETag buffers. Repeated producer Vary fields remain intact. Normal empty
responses apply final header operations when the handler returns.

Compression negotiates against the actual response. It preserves acceptable
origin coding stacks without decoding, and only generates an allowed gzip or
Brotli representation when transformation is permitted. Explicit encoding
exclusions are authoritative: a successful response with no acceptable coding
becomes an empty 406. Malformed Accept-Encoding is an empty 400 before origin
dispatch. Existing bodyless statuses remain bodyless; denials and upstream
errors keep their status for auth/Retry, dropping an unacceptable error body.
An outer compressor selects the representation before ETag evaluates the
original read conditions. Rejected renders retain their 406 status.

Compression owns final Content-Encoding metadata. Configuration rejects raw
response-header Set/Add/Remove operations for that header on a compression route,
including writes through RequestID's configurable output header, regardless of
declaration order. The same check runs on assembled Docker chains
before publication; conflicts use the existing per-route refusal envelope.

Explicit ETag middleware owns a buffered inner representation. It clones a
GET/HEAD request into an unconditional, range-free GET render, hashes successful
200 output, and evaluates the original read preconditions against that result.
HEAD sends headers only; the render stays inside the route and does not re-enter
listener observation. Encoding outside the hashing stage weakens the validator;
hashing outside encoding covers encoded bytes. Representation headers follow the
same ownership on GET and HEAD. Inside an ETag render, compression defers codec
flushes until completion. Encoded bytes and their strong validator are independent
of proxy flush timing. Compression outside that buffered render retains
streaming flushes. Buffered middleware discards informational 1xx
statuses while retaining the final response, and upgrade requests bypass Cache,
ETag, and compression.

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
no middleware, and one fixed 404 refusal. Dispatch is static routes, then existing
Docker dispatch with its container-mutation quarantines and tombstones, then
`Config.FallbackRoutes`, and finally `Config.Fallback`.
Tombstones exist because
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
- one Statute process is the sole lifecycle authority for its governed
  containers; overlapping or rolling authorities for the same workload are not
  supported.

Routing remains the primary concern and lifecycle exists to make a routed service
available. A requirement that needs any of the excluded capabilities belongs
outside this layer.

Statute owns lifecycle intent; Docker state supplies observations. An
external stop is not an administrative disable: after observing it, Statute may
start the container again on demand. An external start may be adopted through
readiness and remains subject to idle shutdown. Event actions do not identify
operator intent or prove an intermediate state that discovery never observed.
There is no cooperative pause, lease, or external administrative-control signal.
Removing eligibility revokes new lifecycle authority while preserving ownership
of an already-issued mutation. The operational conflict matrix lives in
[`docs/docker.md`](docs/docker.md#external-lifecycle-actors).

Discovery subscribes continuously to Docker events and debounces reconciliation.
Every successful subscription, initial or reconnected, requests a full listing
after the subscription is established, closing the listing/subscription gap.
`Refresh == 0` adds no periodic polling; positive `Refresh` supplies an independent
periodic listing. Event-only operation is supported, but cannot promise bounded
discovery of silent stream stalls, undetected delivery loss, or daemon outages.
Neither events nor polling are a second lifecycle owner, and mutation convergence
does not depend on either producing another observation.

A discovered service is not the unit of activation. `mergeService` folds
same-named registrations from several containers into one pool. Such a service
carries one backend plus every backend folded in from its siblings, and takes its
pool-level label settings from the first container. Start and stop act on a
container, so the workload is the per-container contribution beneath a service. A
service with more than one contributor has no single activation owner. On-demand
does not apply to it, and the provider reports that.

The dual holds. A stop acts on the whole container, so a container contributing
more than one service has no single controllable lifecycle: one service's idle
timer would tear the container down while a sibling service still carries
traffic its workload never counted. On-demand requires a one-to-one service and
container pair; anything else is reported and left ungated.

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

Every issued stop has durable mutation ownership. `Docker().Storage(path)` is
required with `Workload`; this existing persistent directory holds a write-ahead
registry bound to the Docker endpoint. Before calling Docker, Statute persists the immutable container ID,
mutation kind, and prepared state with atomic replacement and file and directory
sync. A definitive rejection before any ambiguity proves non-application and
settles the mutation. Ambiguous outcomes remain recorded, and a later rejection
does not erase them; only positive stopped or missing-ID evidence then settles the
mutation. The record is durably removed before ordinary traffic can resume.
Registry I/O holds only the registry mutex. Lifecycle captures the exact stop,
binding pointer/key, and immutable ID, performs durable I/O unlocked, then
revalidates that owner before committing state. Reconcile records stopped evidence
and delegates deletion to tracked convergence after releasing its workload locks.

A fresh process loads this registry before its first Docker route publication.
Each record restores one retired mutation owner and quarantines only the recorded
immutable container contribution while convergence resumes. Current labels,
service names, contributor counts, and backend validity may determine whether a
new mutation is eligible, but never determine historical ownership. A relabelled
recorded container therefore remains quarantined, another contributor stays
independently routable, and a same-name container with a different ID inherits
nothing. Registry open, validation, endpoint-binding, or persistence failure fails
closed at the affected startup or mutation scope. A provider-run restart within
the same process follows the same durable contract.

Activation is single-flight. Concurrent requests for one dormant workload produce
one start operation, one readiness wait, and one outcome delivered consistently to
every waiter. Cancellation is explicit: one client disconnecting does not cancel an
activation the remaining waiters still need.

Activation failure is terminal for the request. A timeout or failure answers the
client, `503` with `Retry-After` where meaningful, and does not continue into
`Config.Fallback`. Operator code that never asked for the workload cannot answer
for it. The original request survives until proxying begins. Backoff is cleared
early only by lifecycle evidence after the failure: the same immutable binding
must be observed stopped and then running, or a replacement binding must be
observed running. A running observation after a definitively rejected cleanup
stop is the known result of that rejection and preserves the backoff. Demand
after the window retries readiness without starting the already-running
container again.

A Docker listing is a snapshot from capture time. Each listing carries the
process-local observation epoch of every workload owner from before Docker I/O.
Activation settlement, stop installation, and terminal stop outcome advance
that epoch. A mismatched snapshot mutates no later lifecycle state and forces a
fresh listing; rejecting only its route publication would be too late because
repair evidence, retirement, binding replacement, and mutation settlement all
occur while the generation is derived. Final publication is also fenced by one
provider-wide observation revision, checked atomically with mutation revisions
and the table swap. A lifecycle transition after observation validation therefore
cannot publish a generation derived from its predecessor state.

Idle is measured from request completion. An in-flight HTTP request, an open
WebSocket, and an open streaming response each hold the workload active, and the
idle timer starts when the last of them finishes. A request arriving while the
workload is stopping has one defined outcome and never proxies into a container
being torn down. Activation success and definitive stop rejection reserve one
binding-scoped activity lease per waiter before idle may arm.

Lifecycle state belongs to the generation that owns it. Docker generations are
replaced atomically, and a retired generation may not mutate or cancel its
successor's state. That holds while a workload is starting, while labels change
during activation, while Statute shuts down mid-activation, and when Docker
reports a stop from outside.

An activation or issued stop is bound to one immutable container identity. If a
reconcile replaces that container beneath the same service, waiters on the stale
operation fail closed, its eventual result is ignored, and a running successor
enters a fresh observe-only readiness attempt. Stale work neither establishes
readiness nor issues cleanup for the successor. Request activity and completion
carry the same identity: an old stream may finish, but cannot hold or arm the
successor's idle lifecycle. Its binding token remains stable when the Docker call
target is refined from a known container's name to its ID.

Three lifetimes remain distinct. The provider allocates an explicit,
provider-lifetime-unique container-incarnation key and keeps request activity
with that binding. The
dynamic table owns a routing revision derived from handler-carried matcher and
middleware semantics; it stays stable while a stopped container materialises its
backend, while a label or middleware change invalidates queued handlers before
they can proxy. A `runningPool` owns health state and transport connections and is
reused only when both its resolved fingerprint and, for a gated workload, its
container-incarnation key remain equal.

Docker call references refine monotonically. Discovery may add an immutable ID to
a name-only binding, and a later observation without that ID cannot erase it. A
Docker mutation remains represented by binding-owned workload state until its
outcome settles. This includes the cleanup stop after a failed activation. A lost
stop response or server error enters a non-serving unknown state even when an
immediate inspect still reports running, because the server-side stop may still
complete. Uncertainty belongs to the whole mutation and is monotonic: a later
rejected retry cannot erase an earlier attempt whose outcome was ambiguous. The
operation owns backed-off retries of bounded calls and coalesced reconciliation
until a successful or already-stopped response, a missing immutable container ID,
or a stopped observation resolves it. Readiness uses the same provider-owned
publication edge and remains probe-paced after publication, keeping global rebuild
work out of per-activation polling.

Lifecycle authority and mutation quarantine have separate lifetimes. If a
one-to-one grant becomes invalid while its stop is already issued, the grant is
revoked immediately and can authorize no new mutation. The existing stop still
owns its retries and a container-wide non-serving quarantine: every service
contributed by that immutable container answers `503` until terminal evidence
settles the stop, including generations where the stopped container has no
materialised backend. Quarantine compiles directly from the already-derived
registration envelope, including one whose ordinary matcher extraction or
serving configuration is invalid. Container provenance remains attached before
service merging: the quarantined contribution is excluded from ordinary pools,
while a different immutable container contributing the same service remains
routable. An unextractable predecessor already owned by an unsettled stop does
not make its independently extractable service-key successor lose one-to-one
authority; an additional successfully extracted contributor still does. Ordinary
routes and quarantine claims share one specificity order;
an independent healthy contributor to the same logical service wins only the tie
between identical `Host`, `HostKind`, `Path`, and `PathKind` predicates. Every
matching tied quarantine requires its own same-service healthy contributor; proof
for one quarantined service cannot neutralize another. A narrower
quarantine therefore cannot fall through to a broader healthy route, while a
narrower healthy route still precedes a broader quarantine. Matched quarantined
claims precede tombstones and answer `503`. Terminal
settlement schedules a coalesced reconcile
that publishes the quarantine's removal without relying on a Docker event or
periodic refresh; only that later generation lets ordinary serving-validation
results and refusal semantics determine the published route outcome.

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

### Cloudflare trusted-proxy ranges

`CloudflareTrustedProxy()` is an explicit listener-owned source policy, separate
from `BehindCloudflare()` TLS behavior and static `TrustedProxy` CIDRs. Resolve,
export, graph, and lint remain offline: the resolved source marker describes
managed acquisition; effective ranges belong to the runtime snapshot.

An optional `FallbackSnapshot` is application-owned input, copied at the surface
boundary and strictly validated into the resolved listener's typed snapshot.
Managed listeners sharing one server must have equal effective fallback ranges
and fetch-time instants; omission selects the bundled snapshot. Server creation
copies the selected fallback, and each startup attempt seeds that owned copy.
Explicit invalid input fails resolution. Generation is optional build preparation;
ordinary builds and configuration tooling perform no network acquisition.

Only a server with that source in use acquires a refresh run. One bounded startup
fetch and one periodic worker serve all its opted-in listeners. Both address
families must validate before an immutable snapshot replaces the previous pair.
Startup failure uses the generated embedded fallback with a warning; background
failure retains the last valid pair, warns, and retries. Provider cache lifetimes
and response age guide the bounded refresh schedule. Static policies never read
this source, and no request performs network acquisition.

The outer listener wrapper snapshots the source once per request, keeping ACLs,
client-IP routing, rate limits, and logs on the same policy even during refresh.
TCP and QUIC share that wrapper. A typed refresh run owns its cancellation,
timer, client, and completion signal; startup rollback and normal shutdown cancel
and join it. No predecessor can publish into a retried startup after rollback.

### Server resources

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

The server shutdown grace period cancels provider-run stop and inspect calls. An
issued mutation cancelled before confirmation remains durably uncertain and
non-serving; the next provider run resumes convergence before route publication.

If a PR claims transactional or retryable startup, tests must prove both resource
release after failure and successful serving after retry. A nil return from the
second `Start` is insufficient.

## Observability

Docker workload diagnostics are owned by the Docker layer. Provider-lifetime
service counters are bounded by the compiled workload policy map and survive
container replacement, grant retirement, and retired-owner pruning. Duration and
safe failure details instead belong to the immutable binding: same-binding
retirement/regrant preserves them, while another container starts fresh. Retained
mutation predecessors stay separately visible; restored owners without a current
configured service are reported without their historical service name.

The existing metrics listener exposes these counters and a read-only JSON snapshot
at `/debug/workloads`. Observability consumes copies, never lifecycle authority.
Snapshot lock order is registry membership, all included owner mutexes, then
diagnostic totals; lifecycle writers already holding one owner take only totals.
No encoding or network I/O holds those locks. No raw Docker identity, endpoint,
label, policy, or error text crosses this boundary. The listener remains an
unauthenticated operational surface that must be bound privately; health and
content listeners gain no diagnostics. The resolved model, export, and graph
continue to describe only static policy.

Listener-level observability wraps the routed content path. Access logging and
metrics use the final response status, including handlers that emit informational
1xx responses before the final status.

Observations run on handler exit, including panic unwinding. They preserve a
committed status and use zero when an aborted handler committed none; they do
not swallow or replace `http.ErrAbortHandler`. Body-byte counts use the counts
returned by writer operations. Aborted and recorded body-I/O-error outcomes are
separate bounded signals, with no response content or panic/error text.
Access-log status filters remain authoritative; within them failed outcomes
bypass sampling even if a 200 response had already begun.

Access logs may describe a verified TLS client certificate from the request's
connection state. Enforcement remains in the TLS handshake.

Response-writer wrappers must preserve interfaces needed by streaming and efficient
copy paths, such as flushing and `io.ReaderFrom`, when the underlying writer
supports them.

The HTTP/3 listener supplies its companion `*http.Server` as the standard
`http.ServerContextKey` value. This enables the standard reverse proxy's
body-copy error abort path for both pool proxies and custom handlers. quic-go
owns panic recovery and converts `http.ErrAbortHandler` into a stream reset;
an interrupted upstream body must not become a successfully completed response.
The context bridge acquires no resources and leaves socket ownership unchanged.

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
