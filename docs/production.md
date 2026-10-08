# Production deployment

This document covers the operational concerns of running statute in production: how to bind low ports without root, signal handling and graceful shutdown ordering, persistent storage requirements, build flags, and the deployment patterns we recommend (and the ones we don't).

## Building the binary

### Release availability

The release workflow runs vet, race tests, and example builds before publishing
the GitHub release and warming the Go module proxy. A separate verification job
then requests the exact tag from pkg.go.dev using its Request-button endpoint
(`POST /fetch/<module>@<tag>`) before polling fresh version metadata for up to
20 minutes. The request has bounded retries; if it fails or times out, polling
still checks whether indexing completed. Only the indexed tag and the existing
metadata checks can make verification pass, not the fetch request alone.

Documentation indexing can lag behind module availability. A verification
timeout does not undo the published release or its Go proxy availability. Once
indexing is available, rerun only the verification job; do not move the tag or
republish the release. Reruns use the workflow from the original tagged commit,
so workflow fixes on `master` apply to future tags, not existing releases.

### Build commands

```sh
go build -o statute ./examples/basic
```

The binary is a normal Go executable. There are no runtime dependencies on the build machine — no shared libraries to ship, no config files to colocate, no template directories. Your `main.go` and the resulting binary are the entire deployment artefact.

For smallest binaries:

```sh
go build -trimpath -ldflags="-s -w" -o statute ./examples/basic
```

`-trimpath` removes filesystem paths from the binary; `-s -w` strips DWARF debug info. Typical savings: ~25%. Skip these if you want stack traces with line numbers and absolute paths.

For static linking:

```sh
CGO_ENABLED=0 go build -o statute ./examples/basic
```

statute does not use cgo itself, but Go's default DNS resolver does in some configurations. `CGO_ENABLED=0` forces Go's pure-Go resolver, which produces a fully-static binary you can run from `scratch` containers.

## Streaming failures

If a proxied response body fails after response headers are committed, Statute
aborts that response. HTTP/2 and HTTP/3 reset the affected stream; HTTP/1.1 closes
the connection. Clients must treat a body-read error as an incomplete response,
even if the already-received status was 200. A committed response cannot be
replaced with a new error status. This also applies to standard Go reverse
proxies supplied through `Handle` routes.

## Response cache

Put `AllowIPs(...)` and `DenyIPs(...)` **before** every enabled `Cache(...)` in
the route's `With(...)` list. Cache hits skip inner handlers, so the opposite
order is a configuration error. IP checks run on cache hits and misses.
For example, use `.With(AllowIPs("192.0.2.0/24"), Cache("1m"))`.
This also applies to fallback routes and assembled Docker middleware: defaults
come before named chains in label order. An unsafe Docker combination refuses
that router without affecting siblings. Disabled caches (`Cache("0s")`) do not
impose this restriction. Middleware declaration order otherwise stays unchanged.
`RequestID()` must likewise precede every enabled Cache. It runs on each request
and Cache selects variants using already-mapped headers. RequestID owns its
response field at commitment, including cache replay; explicit route response-header
operations retain outer precedence. See [request identity](request-id.md) for
the complete reserved output set and the Authorization/Cookie exceptions.
Place RateLimit
before Cache to limit every request, or after Cache to limit only misses; both
orders are supported. Routes that require custom authorization inside `Handle`
on every request must omit Cache or move that check outside the cached handler.
Response private/no-store controls storage; it does not run skipped handlers on
a cache hit.

See [Cache and per-request policy](cache-request-policy.md) for the complete
middleware/observation matrix and custom-authorization revocation examples.

`Cache(ttl)` is an opt-in, route-local response cache for 2xx GET/HEAD responses,
keyed by method, host, original and rewritten request targets, URL scheme,
downstream TLS presence, and response `Vary` selection. The configured
TTL controls expiry. Each stored variant records a copy of its selecting request
header values, distinguishing absent from empty fields. Comparison is exact;
equivalent but differently spelled field values can cause an extra miss.

Routes using CORS always retain `Vary: Origin` in cache selection and final
response headers. This holds in both CORS/Cache orders and through Retry/ETag;
raw Vary replacement or removal cannot remove this CORS-owned dimension. Other
producer variance still participates in cache selection. Routes without CORS
retain their own independent variance policy.

Requests carrying `Authorization` or `Cookie` bypass both lookup and storage,
even with `public` or `s-maxage` response directives. This includes empty fields
and uses the effective request after route request-header operations. These
requests cannot consume or replace a warm anonymous entry. Responses containing
`Set-Cookie` or `Cache-Control: private` are never stored. Qualified `private`
directives also prohibit the entire entry; Cache does not strip named fields to
make a private response shareable.
Requests with a presented TLS client certificate or verified client chain also
bypass lookup and storage, including optional or application-verified certificates.
An application's certificate-specific response cannot populate or consume an
anonymous entry. HTTPS requests without client certificates remain cacheable.
This does not change listener-owned TLS authentication or create per-user caches.
Response `no-cache`, including qualified forms such as `no-cache="X-Secret"`,
also prevents storage. Responses are delivered unchanged; Cache does not perform
the validation required for subsequent reuse. The rule applies to both origin
and projected response headers, and removing origin no-cache cannot allow storage.
Configure at most one `RequestID()` per route, even when different output headers
are used. Static and fallback duplicates are configuration errors. For Docker,
the limit includes defaults and every referenced named chain: a duplicate in the
assembled chain refuses that router while valid siblings remain available.
Choose either the shared default or the router-specific declaration. Placing
Retry outside the single RequestID still allows a new ID per attempt.

Routes using `RequestID().Header("Authorization")` or `.Header("Cookie")`
bypass Cache entirely, regardless of declaration order. Those credential writers
can run inside request clones that an outer Cache cannot inspect. This policy
also applies to Docker-assembled chains without affecting sibling routes.

Request or response `Cache-Control: no-store` prevents storing a new entry.
Every field value is checked, with case-insensitive directive names and quoted
extension arguments handled correctly. Malformed directives also skip storage;
the response is still served normally. Request no-store can use an existing
entry; it does not purge that entry or force origin revalidation, as specified by
[RFC 9111 section 5.2.1.5](https://www.rfc-editor.org/rfc/rfc9111.html#section-5.2.1.5).

Route response-header operations are hoisted outside Cache and Retry. Cache
checks both the downstream response and a copy with those ordered operations
applied. A route-added no-store therefore prevents storage. Removing or replacing
an origin no-store, private, or Set-Cookie header cannot authorize storing that
response. Projected private or Set-Cookie also prohibits storage. Actual response
headers are still applied once when the final response is committed.

Conditional headers (`If-Match`, `If-None-Match`, `If-Modified-Since`,
`If-Unmodified-Since`), `Range`/`If-Range`, request `no-cache`/`no-transform`, and
malformed request Cache-Control bypass both lookup and storage. Downstream
middleware or the origin must evaluate them; a warm entry cannot bypass that
policy. These requests do not purge an existing unconditional entry.

GET/HEAD requests with a positive or unknown body length, transfer encoding, or
request trailers also bypass lookup and storage. Cache does not read, replace,
or close the request body. Empty HTTP/1 and HTTP/2 requests remain cacheable.
All HTTP/3 requests currently bypass Cache, including `Content-Length: 0`:
quic-go exposes unannounced trailers during body reads, after cache lookup.
Future non-consuming HTTP/3 body-and-trailer detection is tracked in
[#165](https://github.com/kjanat/statute/issues/165).

Vary selection uses the union of origin and projected route response-header
operations, including any origin fields removed by route configuration.
Repeated/case-insensitive field names are normalized. `Vary: *`, invalid Vary,
206 responses, and responses carrying Content-Range are delivered without
storage. A changed Vary field set replaces incompatible variants for that key.
Compression declares `Vary: Accept-Encoding` even for identity responses; both
Cache/Compress orders preserve encoded versus identity selection.

Native proxy routes additionally bypass Connection-bearing requests and decline
storage when Vary selects forwarded or OpenTelemetry-injected fields. A captured
propagator that can inject credentials or other request controls also bypasses
caching. Propagator field-set changes partition entries; drift during an attempt
disables further reuse and storage for that request. Ordinary application-header
Vary variants still work. See [representation identity](cache-request-policy.md#representation-identity-and-native-proxies)
for the full contract and custom-handler limitations.

For a temporary fail-open HTML-rewrite bypass, the original response is delivered
with no-store and discarded after delivery. The next request reaches the origin
again; a supported, successfully rewritten response can then be cached normally.

Cache has finite per-instance entry, body and allocation limits. It does not
implement per-user caches, conditional revalidation of stored entries or stale
serving. Use it only for responses safe to share under their selected Vary keys.
Avoid it for personalized or streaming
routes. Applications using custom identity headers or request-context identity
must mark personalized responses private/no-store or omit Cache; arbitrary
application identity cannot be inferred. Use a suitable cache implementation for
broader HTTP caching needs.

### Cache freshness

Origin freshness selects the first applicable value: `s-maxage`, `max-age`, then
`Expires - Date`. The configured TTL is an additional upper bound on local
residence, starting at the first final response headers. Without explicit origin
freshness, the configured TTL remains the lifetime. `must-revalidate` does not
disable fresh hits, but an expired response is never reused, even with
`stale-while-revalidate` or `stale-if-error`.

Corrected age is the greater of apparent age (`receipt - Date`, at least zero)
and incoming `Age` plus request/response delay. Buffering, cache residence and
slow delivery continue consuming freshness; publication does not restart the
clock. For example, `max-age=60` with `Age: 50` has at most ten seconds left,
even with `Cache("1h")`. Hits emit the current `Age`; missing `Date` is supplied
from receipt time. These calculations follow [RFC 9111 section 4.2](https://www.rfc-editor.org/rfc/rfc9111.html#section-4.2).

Malformed or duplicate effective freshness values, ambiguous `Date`/`Age`, and
freshness fields declared as trailers prevent storage without rejecting the
producer's response. A valid `s-maxage` overrides lower-priority `max-age` and
`Expires`; invalid selected values do not fall through to a weaker policy.

ETag, Retry and Timeout buffering preserve the producer's original commitment
time. Changes to committed freshness policy disable storage for the enclosing
cache request, including discarded Retry attempts; response delivery is unchanged.

Origin and projected route policy both constrain expiry. Setting a longer
`max-age` or removing origin policy cannot extend its freshness. Setting a shorter
lifetime can shorten it. Explicit response `Age` or `Date` Set/Add/Remove
operations bypass every Cache on that route, including fallback and Docker
routes, to preserve the configured header ownership.

Requests carrying `max-age`, `min-fresh` or `max-stale` bypass lookup and storage;
the producer handles them. Existing conditional/range/no-cache/no-transform
bypasses remain unchanged. Request `no-store` prevents new storage but does not
invalidate an otherwise eligible existing hit.

### Cache capacity

The defaults are **1,024 entries**, **8 MiB per buffered response body**, and a
**64 MiB allocation budget** for each compiled Cache middleware. Configure them
independently for each route:

```go
Cache("1m").MaxEntries(2048).MaxResponseBody("4MiB").BufferBudget("128MiB")
```

Each Vary variant counts as an entry. Active misses and retired entries still
being read also consume entry capacity. Entries are removed from lookup on expiry,
replacement or eviction; a slow response retains its allocation charge until its
last reader finishes. Every cache operation sweeps expired entries across all
keys. No background cleanup worker is started. Separate middleware instances,
including routes sharing one upstream pool, have independent budgets.

Cache accounts body capacity, overlapping allocations during growth, retained
key/header/Vary metadata, and a conservative **512 KiB scratch reservation** for
each active cache attempt. That reservation covers bounded request snapshots,
response-header projection, variant selection and replay copies. A budget smaller
than 512 KiB always takes the uncached path. The budget measures allocation
ownership. Process RSS also includes producer-controlled `Header()` maps, other
middleware, tracing propagator internals, allocator overhead and garbage awaiting
collection. Native-route propagation snapshots have separate fixed ceilings of
256 declared fields and 16 KiB of field names, checked before copying.

Fixed admission ceilings are 16 KiB of key input, 64 KiB of accounted headers,
256 header names, 1,024 header values, and 256 Vary tokens. Large request metadata
or unavailable initial capacity goes directly to the producer. Oversized response
bodies or metadata and unavailable growth capacity switch permanently to
uncached streaming: the buffered prefix is delivered once, then writes continue
downstream. Producer status, errors and trailers are preserved. Cache capacity
does not produce a new 502/503, cancel the request or trigger a retry.

`Flush` remains deferred while the response fits in the buffer. Unknown-length
proxy responses, including HTML-rewritten responses, routinely request flushing
and remain cacheable. After overflow, flushing is forwarded to the downstream
writer. Streaming routes that need immediate delivery should omit Cache.

The same rules apply through named/default Docker middleware and fallback routes.
Changed resolved limits participate in normal Docker generation replacement.

## Buffered response limits

ETag and Retry default to **8 MiB per rendered response body** and a **64 MiB
response-body allocation budget per compiled middleware instance**. Configure
them independently:

```go
ETag().MaxResponseBody("16MiB").BufferBudget("128MiB")
Retry(3, OnStatus(503)).MaxResponseBody("4MiB").BufferBudget("32MiB")
```

Sizes must be positive; the budget must be at least the body limit. There is no
unlimited setting. The limits apply to static, fallback, and code-owned Docker
middleware. Export includes both normalized byte counts. Separate routes and
repeated middleware declarations own independent budgets.

Oversize returns 502; insufficient budget returns 503. No partial producer body,
headers, trailers, or generated ETag are delivered. The failure carries no-store
and an empty body at this middleware's output; an outer compressor can add codec
framing. HEAD performs the same bounded ETag render. An incomplete representation
cannot satisfy a conditional request. The owning Retry stops its attempt loop;
an outer Retry may retry the failure if its configured status policy selects it.

Budget accounting includes allocated body capacity, concurrent renders, slow
downstream replay, and old plus new allocations during buffer growth. Growth can
therefore require more budget than the final body size. Retry releases each
attempt before starting another. Allocation exhaustion fails immediately; there
is no queue or temporary disk spool. Overflow cancels the render's child context
and fails further writes; custom handlers must respect write errors/cancellation.

Each limit measures bytes at that stage: ETag outside compression buffers encoded
bytes, while ETag inside compression buffers identity bytes. Nested middleware
charge their own copies. These are **not whole-process memory limits**: headers,
request copies (including Retry's separately capped request body), codec memory,
allocator overhead and garbage awaiting collection are excluded. Inner Cache or
Timeout can buffer before the bounded writer sees any bytes. Cache has its own
independent limits described above. Timeout owns the separate bounds below.
Retry request-body retention also has its own budget.

## Timeout bounds

Timeout defaults to **8 MiB per response**, a **64 MiB body-allocation budget**
and **128 in-flight producers per compiled instance**:

```go
Timeout("30s").MaxResponseBody("4MiB").BufferBudget("32MiB").MaxInFlight(64)
```

Body sizes must be positive and the budget must cover the body limit.
MaxInFlight accepts positive values; zero selects 128 and negative values fail
resolution. Static, fallback and Docker routes have independent instances.
Native `statute.timeout` labels use defaults; named code-owned Docker middleware
can select explicit limits. Export carries all normalized values.

Oversize returns empty 502. Exhausted body budget or producer capacity returns
empty 503 without queueing. Resource failures carry `Cache-Control: no-store`
and discard producer headers, body and trailers. An outer Retry retains its
configured failure policy. Ordinary deadline expiry remains 503 with
`request timed out`; parent cancellation returns empty 503.

A timed-out handler that ignores cancellation keeps its producer slot and body
charge until it actually exits. Successful buffers remain charged throughout
downstream replay, and growth charges old and replacement allocations together.
Late deadline writes return `http.ErrHandlerTimeout`; cancelled writes return
the context error. Inner Retry request-body leases survive the same late-producer
lifetime. Producer header maps, other buffers, codecs and process RSS are outside
these body limits. Cancellation cannot forcibly terminate application code.

Timeout does not stream, flush or hijack. HTTP/2 `Push` returns
`http.ErrNotSupported` immediately, including when the downstream writer supports
push: forwarding it could block deadline return or use a writer after its request
has finished. Successful responses preserve trailers; informational statuses are
discarded until the final status. Empty completion remains 200. Application panics
and unrelated proxy aborts propagate while Timeout is waiting for the producer.

## Retry request-buffer limits

Retry has a separate **64 MiB request-body allocation budget per compiled
instance**. Configure it independently of its response limit and response budget:

```go
Retry(3, OnStatus(503)).RequestBufferBudget("16MiB").BufferBudget("32MiB")
```

The existing **1 MiB replay ceiling** remains. Sizes must be positive and fit in
an int; small budgets are allowed. An allocation is reserved before the first
body read, including readers that block without returning bytes. Growth accounts
for both the old and replacement buffers. A budget must cover that temporary
overlap to keep buffering, even if the final body alone would fit.

When initial capacity is unavailable, Retry forwards the untouched body once.
When growth cannot be admitted or the body exceeds the replay ceiling, it forwards
the buffered prefix followed by the unread original stream once. Those paths keep
the producer's response and original body-close ownership. They introduce no
status code or retry; response-budget 502/503 failures remain separate. A request
body read error still returns 400. Non-idempotent and streaming/upgrade exclusions
continue to bypass request buffering.
Streaming fallback keeps its EOF trailer values attached to the body through
BodyLimit and native proxy/rewrite clones. Native forwarding selects streaming
framing when the body can carry trailers, as described below.

The allocation stays charged through all attempts and final response delivery.
If an inner Timeout returns before its producer stops, that producer retains a
lease on every inherited Retry request buffer. Native upstream transports also
retain their leases until they have closed the request body and finished any
in-progress reads, even when an early upstream response has already completed.
Transport replay bodies each retain their own lease. The charge is released
after the last actual owner finishes, including nested Retry/Timeout and panic
cleanup. Cancellation alone cannot reclaim a still-used buffer. Custom handlers
retaining bodies beyond their handler lifetime must manage that retention
themselves.

Static, fallback and Docker middleware use the same normalized
`RequestBufferBudgetBytes` field. Separate routes and repeated Retry instances
have independent budgets, including routes sharing one backend pool. These limits
cover Retry-owned body allocations; incoming transport buffers and arbitrary
custom-handler allocations remain separate.

## Live request trailers

Statute preserves transport-exposed request trailers through listener context
wrappers, path rewriting, Timeout, ETag, BodyLimit and Retry. Read the body to
successful EOF before consulting `Request.Trailer`. Partial reads, read errors
and closing an unread body do not publish final values. Statute does not consume
input early to discover trailer names or values.

Native `ProxyTo` routes, including Docker routes and the private rewrite
experiment, preserve those trailers on their outgoing request. That private
request uses streaming framing for trailer-capable bodies: chunked HTTP/1 or
HTTP/2 trailing HEADERS, including empty HTTP/3 input with Content-Length: 0.
Incoming ContentLength and transfer encoding stay unchanged for handlers and
cache admission. HTTP/3 continues to bypass Cache as tracked in #165.

Unannounced fields are forwarded on the wire, but the receiving application may
not expose them: Go's HTTP/2 server makes only announced trailer names available
to handlers. Declare trailer names when the receiving application requires it.
Likewise, Statute cannot recover fields its ingress transport never exposes.

Trailer ownership follows the body. A custom handler replacing the body must
provide the replacement's own trailer/framing semantics. A custom proxy supplied
through `Handle` owns its request cloning and transport; unlike native `ProxyTo`,
it must itself preserve live trailers when deep-cloning a request with a shared
body. Do not copy trailer fields into ordinary headers to work around this.

## Body-derived ETags

`ETag()` buffers the inner GET response and hashes successful status-200 bytes.
For HEAD it clones the request, renders that same inner GET, computes the same
validator, and sends headers only. This can do the full work of a GET; its body
buffer is bounded as described above. It remains one external HEAD in access logs
and request metrics.
Request authentication, negotiation, context, and route selection are retained.
The render clone removes read preconditions and ranges; the original request is
evaluated against the completed representation afterward. Matching If-None-Match
(including weak/list/wildcard forms) yields 304; a failed strong If-Match yields
412. Invalid entity-tag syntax yields 400. Date conditions obey precedence and
are ignored when the selected response has no valid Last-Modified value.

Body-bearing ETag responses retain unrelated declared and late trailers. When
trailers are present, ETag omits Content-Length so HTTP/1.1 can use chunked
delivery. Generated ETag and length metadata cannot be replaced by origin
trailers. HEAD and conditional bodyless responses omit the synthetic render's
trailers entirely, including their values.

The first declared middleware is outermost. `With(ETag(), Compress(Gzip))`
hashes encoded bytes and supplies a strong tag for that encoding.
Inside that buffered render, compression defers intermediate flushes until
completion: proxy scheduling cannot change the compressed bytes or their tag.
Compression without an outer ETag still flushes progressively.
`With(Compress(Gzip), ETag())` hashes identity bytes and supplies a weak tag for
compressed delivery. Both produce GET/HEAD-equivalent validators at that pipeline
position. Outer compression omits the identity Content-Length on HEAD; it is not
the encoded length. HEAD and 304 responses carry no compressed body.

Compression preserves acceptable origin encodings without decoding, including
codings it cannot generate itself. Partial responses and request/response
`no-transform` prevent new encoding; malformed cache directives do too.
It removes identity length/digests/range support when encoding starts.
Announced and late representation trailers are also removed after encoding;
unrelated trailers and untouched bypass responses retain their trailers.
An aborted handler propagates its panic without finishing a compressed stream.

Routes using enabled `Compress(...)` must not Set/Add/Remove the response `Content-Encoding`
header: compression owns that metadata for the bytes it generates or preserves.
`RequestID().Header("Content-Encoding")` is always rejected, including without
compression, as part of its reserved output-header policy.
Resolve rejects these combinations in either declaration order, including fallback
routes and Docker middleware definitions. Docker also validates the complete chain
after combining defaults, named middleware, and label hints; a conflicting route
is refused without affecting valid siblings or falling through to a fallback.
Removing `ETag` or `Content-Length` remains allowed, as do unrelated headers and
request-header operations. Header operations on routes without compression are
unchanged. An empty algorithm list disables compression and introduces no conflict.

`Accept-Encoding` is parsed across all field lines, case-insensitively, with
quality values and wildcard/specific exclusions. Gzip and Brotli are selected by
quality; Brotli wins ties. Explicit identity preference can win over an encoder;
otherwise identity is the fallback when allowed. No field or an empty field
keeps an identity origin response unencoded. An absent field also permits any
existing origin coding, while an empty field permits identity only. Repeated
codings use their lowest quality; exclusions take precedence over higher duplicate
weights. `x-gzip` is treated as `gzip`.

Qualities allow zero to three decimal places in the 0 to 1 range; the leading-dot
form `.5` is accepted as an interoperability extension. Invalid tokens, weights,
or extra parameters produce an empty 400 before calling the inner handler.
If no acceptable representation can be produced or preserved, an otherwise
successful response becomes an empty 406: for example, `zstd, identity;q=0`
with an identity origin and only gzip/Brotli available. An acceptable origin
zstd response passes through. Response headers always include
`Vary: Accept-Encoding`, including identity and rejection paths.

Already-bodyless statuses such as 204 and 304 require no payload coding and keep
their status. Upstream errors and auth denials also keep their status so Retry
and authentication retain their meaning; an unacceptable error body is omitted.
With explicit ETag rendering, negotiation precedes evaluation of the original
conditions. Rejected renders retain 406 even with `If-None-Match: *`.

Cache can sit on either side: an inner Cache can supply the GET render, while an
outer Cache delegates conditional requests to ETag. Upgrade requests bypass these
representation stages. A render error/panic publishes no generated validator or
partial buffered content. Choose streaming delivery without ETag when buffering
or HEAD rendering cost is undesirable.

## Running on low ports as a non-root user

Binding to ports below 1024 (`:80`, `:443`) traditionally requires root on Linux. Three options, in order of preference:

### Option 1: setcap

Grant the binary the `cap_net_bind_service` capability:

```sh
sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/statute
```

The binary can now bind low ports as any user. This is the standard mechanism on modern Linux. Capability persists across the binary file but is lost on file overwrite — re-run after every deploy.

### Option 2: systemd

systemd's `AmbientCapabilities` injects the capability without modifying the binary:

```ini
[Service]
ExecStart=/usr/local/bin/statute
User=statute
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
```

Combine with `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `ReadWritePaths=/var/lib/statute` for a hardened service. See `man systemd.exec` for the full list.

### Option 3: lower the unprivileged port floor

```sh
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/99-statute.conf
sudo sysctl --system
```

This makes ports 80–1023 unprivileged for all processes. Convenient but global; affects every binary on the host.

### Don't run as root

Don't. There is no good reason. The proxy handles untrusted input from the network on every connection.

## Persistent storage

AutoTLS (every challenge policy) requires persistent storage for the ACME account key, issued certs, and renewal state. The directory layout depends on the policy each source declares:

```text
<storage>/
├── acme_account+key            # autocert account state (automatic policy)
├── example.com                 # autocert cert files (automatic policy)
├── api.example.com
├── dns01/                      # CloudflareDNS01() sources
│   ├── account.key
│   ├── example.com.crt
│   └── example.com.key
└── http01/                     # HTTP01()-pinned sources
    ├── account.key
    ├── api.example.com.crt
    └── api.example.com.key
```

The flat files at the root belong to autocert, which backs the automatic policy (TLS-ALPN-01 attempted first, HTTP-01 as fallback). Each pinned source instead issues through statute's in-tree ACME manager, which keeps its state under a subdirectory named for its challenge.

**Each subdirectory carries its own ACME account key**, registered independently of the autocert account at the root. Persist the whole storage root: losing any one of these directories means a fresh account registration and a fresh issuance for the sources it served.

The standard mistakes that get deployments rate-limited:

- Mounting the storage path as `tmpfs` (cleared on reboot).
- Wiping the storage path during container builds (clean slate on every deploy).
- Forgetting to mount the storage path at all when running in containers.
- Using `/tmp` (cleared periodically by systemd-tmpfiles).

Let's Encrypt rate limits new orders to 50 per registered domain per week. A misconfigured deployment that re-issues on every restart hits the limit in about 30 minutes if you deploy continuously. The fix is always the same: persistent storage, mounted at the same path on every restart, owned by the user statute runs as.

For containers, mount a named volume:

```yaml
volumes:
  - statute-certs:/var/lib/statute/certs
```

Or a host bind mount in development:

```yaml
volumes:
  - ./certs:/var/lib/statute/certs
```

For Kubernetes, use a `PersistentVolumeClaim`. Don't use an `emptyDir` — it disappears when the pod restarts.

## Signal handling and graceful shutdown

statute installs handlers for `SIGINT` and `SIGTERM`. On either signal:

1. The signal context cancels.
2. Readiness turns off and one `Shutdown.GracePeriod` deadline begins.
3. Workload idle stops are disabled; HTTP and HTTP/3 listeners drain in parallel under that deadline.
4. The Docker provider cancels and awaits its watcher, reconcile work, and lifecycle HTTP calls without adding a second timeout. Any issued mutation whose response was interrupted stays durably uncertain for restart recovery.
5. Health checkers stop, idle upstream connections close, and the health listener closes last.
6. The OTel TracerProvider flushes pending spans under the same deadline.
7. The process exits.

Any outstanding request gets up to `GracePeriod` to complete. Requests that don't complete in time have their connections force-closed. The default is `30s`, which is fine for typical web traffic; tune up for long-running endpoints (file uploads, server-sent events, video streaming) or down for fast-deploy environments where you want pods to terminate quickly.

`Shutdown.DrainListeners: true` (recommended) causes listeners to stop accepting new connections immediately, while existing connections finish their requests. Without it, the listener closes hard and in-flight requests get TCP RST.

If statute is in a Kubernetes pod, set `terminationGracePeriodSeconds` >= `Shutdown.GracePeriod` + 5 seconds. Kubernetes sends SIGTERM, waits `terminationGracePeriodSeconds`, then sends SIGKILL. If the pod's grace period is shorter than statute's, you'll see KILLed processes losing in-flight requests.

## Containers

```dockerfile
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /statute ./examples/basic

FROM gcr.io/distroless/static-debian12
COPY --from=builder /statute /statute
USER nonroot:nonroot
EXPOSE 80 443
ENTRYPOINT ["/statute"]
```

`distroless/static` is the smallest sensible base for a Go binary: no shell, no package manager, no /tmp clutter. The `nonroot` user is UID 65532. For low-port binding, use the setcap or sysctl approach on the host running the container, not inside the image.

If you're on Kubernetes, the container needs `securityContext.capabilities.add: [NET_BIND_SERVICE]` and `runAsUser: 65532`. Or use ports >= 1024 inside the container and remap externally with a Service.

## Terminal routing policy

Use [`FallbackRoutes`](routing.md#terminal-native-routes) for a native pool-backed
default page or host-scoped legacy application after Docker discovery. Do not put
an unconstrained `Match("/*")` in ordinary `Routes` for this purpose: it wins before
Docker and shadows both fallback stages (`FB001`). A final terminal catch-all is
intentional operator policy; constrain it by host or client range when unknown
traffic should retain the final 404.

Configure timeout, upstream Host/TLS, and health policy on the named pool. Both
tables reuse its one transport and lifecycle; keep auth, caching, and rewrites on
the route that needs them. `AUTH001` and `RL001` audit terminal middleware too.
Inspect `-export` and `-graph` to verify the separate terminal table and shared pools.

Only **routing misses** reach fallback. A matched backend failure, denial,
404, or 5xx is final. Docker tombstones and mutation quarantines refuse before
either fallback stage; do not use the default page as a workaround for missing
auth references or unresolved lifecycle operations. Listener trust, HTTP-01
challenge ownership, final-status logs/metrics, and shutdown draining still apply.

## Reverse proxy chains

statute is fine as the front door, but it also works as a middle layer. Common patterns:

**Cloudflare → statute → backends**: use `BehindCloudflare()` for the Cloudflare ACME behavior and `CloudflareTrustedProxy()` for direct-peer-verified client attribution. Only listeners using the managed policy trigger startup and periodic IPv4/IPv6 acquisition; provider cache lifetimes and response age guide refresh timing. Startup failures use an embedded fallback, later failures retain the last valid pair, and both warn. Explicit `TrustedProxy` CIDRs remain available without fetching. Cloudflare account ownership requires separate authentication, and direct connections remain possible. See [Cloudflare deployment and refresh behavior](cloudflare.md#managed-cloudflare-ranges).

**ELB/ALB → statute → backends**: similar to the Cloudflare case, but trust `X-Forwarded-For` instead of `CF-Connecting-IP`. Declare the load balancer's address range as a trusted proxy on the listener — `TrustedProxy("10.0.0.0/8")` (the default header is `X-Forwarded-For`) — and the access log, rate limiter, IP lists, and IP-hash strategy key on the real client IP whenever the direct peer is the LB, while any other peer is attributed by its own address. Use static cert mode in this case (the load balancer terminates TLS).

**Service mesh sidecar (Envoy/Linkerd) → statute → backends**: don't. The sidecar already proxies. Either remove statute or remove the sidecar — running both is purely overhead.

**statute → another statute**: works fine, but suggests your routing topology is more complex than it needs to be. Reach for a single statute with named upstream pools instead of chaining instances.

## Observability checklist

Before considering a deployment "production":

- [ ] Access log destination is not stdout in production (parse, ship, retain). Use a structured log aggregator that can index `status`, `path`, `remote`.
- [ ] Metrics endpoint scraped by Prometheus or equivalent. Alerting on error rate (>1% 5xx over 5 minutes) and latency (p95 over SLO).
- [ ] Tracing exported to an OTel collector with at least 7 days of retention.
- [ ] Dashboards for: request rate, error rate, latency by route, upstream pool health.
- [ ] Runbook for common alerts: how to identify the bad backend, how to read the access log, how to inspect a trace.

## Security checklist

- [ ] `ReadHeaderTimeout` set in `Defaults`. Not zero. Slowloris mitigation is the most-skipped, most-impactful production setting in Go HTTP servers.
- [ ] Listener bound to a specific interface in single-tenant deployments (`192.0.2.10:443` rather than `:443`) so an unintended interface doesn't expose the proxy.
- [ ] Metrics listener private. Bind to loopback or a private VLAN. The `pprof` endpoints are debugging gold for an attacker.
- [ ] AutoTLS storage mode `0700`, owned by the statute user. The account key in there can issue arbitrary certs.
- [ ] Cloudflare API token (DNS-01 mode) in an env var or secrets manager, never in the binary or config.
- [ ] Cloudflare client-IP headers are peer-verified on any directly reachable listener. Use `CloudflareTrustedProxy()` for startup and periodic refresh; monitor fallback/refresh warnings. Generate the embedded fallback before release with `make generate-cloudflare-cidrs`, review it, and check with `make check-cloudflare-cidrs`. Runtime range updates do not require a rebuild.
- [ ] No secrets in the source. The Go config-as-code model makes this tempting; resist. Use env vars (`os.Getenv`) for tokens and credentials.
- [ ] Downstream TLS policy reviewed. The floor is TLS 1.2 and nothing lowers it, but `statute.TLSPolicy{MinVersion: statute.TLS13}` raises it where every client is modern; pin `CipherSuites` only when a compliance regime names them, since the list applies to TLS 1.2 alone.
- [ ] Client authentication is fail-closed where required. Use `ClientAuth{Mode: RequireAndVerifyClientCert, CAFiles: ...}`; `TLS007` warns when a softer mode admits an absent or unverified peer. CA files must be readable before startup.
- [ ] CSP / HSTS / X-Frame-Options on responses where appropriate. statute doesn't set these by default; add a small middleware if your backends don't.
- [ ] Rate limiting on every public route. The token bucket is per-key; pick `ClientIP` (or `HostHeader` for tenant-isolation patterns) deliberately.

## Capacity planning

Some rough numbers from the standard library underneath:

- A single `http.Server` handles tens of thousands of concurrent idle connections without breaking a sweat.
- HTTP/2 connection pooling means the upstream transport can saturate a few hundred backend connections from a single statute instance.
- Active health checks add `n_backends * (1 / interval)` background requests. With 10 backends at 10s interval, that's 1 RPS — negligible.
- Each compiled RateLimit instance retains at most 65,536 keys by default. Use `RateLimit("100/min").MaxBuckets(8192)` to tune this per-route bound; zero selects the default and negatives fail resolution. Keys retain fixed-size hashes, so long Host values do not increase retained bucket size. Only fully replenished buckets may retire: churn cannot reset a client's token debt. At capacity, existing keys retain normal token behavior (including 429 with `Retry-After: 1`); a new key without a reclaimable slot receives 503 with `Cache-Control: no-store`. Size the cap for the active client population and refill horizon: attackers can occupy slots and deny admission to new clients until buckets refill. Routes and Docker generations own independent stores even when sharing upstream pools; there is no process-wide quota. RateLimit outside Cache counts requests, inside Cache counts misses, and inside Retry counts each invocation. Outer Retry can retry capacity 503 responses when configured to do so.

For real numbers, run a load test against your specific config. The Go runtime has a lot of variance based on GC pressure, GOMAXPROCS, and cgo invocation patterns; benchmarks from someone else's deployment rarely transfer.

## What we don't recommend

**Running statute under another reverse proxy that also terminates TLS**: pointless. Pick one TLS terminator. If it's Cloudflare, terminate at Cloudflare and use `BehindCloudflare()` here. If it's statute, point DNS at statute directly.

**Hot-swapping the binary**: Go's binary is immutable on disk while running. Replace + restart, with the new binary providing the same listener config so connections drain into the old process while the new one accepts. Use systemd, Kubernetes rolling updates, or a process manager that does the dance.

**Running multiple statute instances on one host without coordination**: each instance owns its own ACME account and storage. Two instances trying to issue for overlapping domains will race on the storage and confuse Let's Encrypt. Run one statute per host, or shard storage and domains carefully.

**Burying secrets in the binary**: the Go compiler does not encrypt strings. `strings yourbinary | grep -i token` finds anything you embed. Read secrets from env or a secrets manager at startup.
