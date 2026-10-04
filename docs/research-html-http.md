# HTML rewriting: HTTP integration experiment

The private adapter in `research/htmlrewrite/http.go` applies the existing fixed
LOL HTML policy to HTTP responses. It adds no production middleware, public
builder, Docker labels, or resolved-model fields. This is the HTTP experiment
for [#114](https://github.com/kjanat/statute/issues/114).

## Consumer-selected failure policy

Each route adapter must explicitly choose `failClosed` or `failOpen`. Zero and
unknown policies are rejected during construction. Two routes can share the
same HTTP transport and compiled engine while choosing different policies.

| Outcome                                                                    | Fail closed                      | Fail open                                              |
| -------------------------------------------------------------------------- | -------------------------------- | ------------------------------------------------------ |
| Selected response is supported                                             | Rewrite                          | Rewrite                                                |
| Unsupported charset/encoding, no-transform, trailers, known oversized body | Reject before consuming the body | Pass the original body and mark it no-store            |
| No rewrite capacity or initialization failure                              | Reject                           | Pass through if the original response remains readable |
| Failure after body rewriting begins                                        | Terminate                        | Terminate                                              |
| Response outside the experimental eligibility scope                        | Pass through                     | Pass through                                           |
| Upstream sends unsolicited 206 or 304 after conditions were removed        | Reject as protocol error         | Reject as protocol error                               |

The reverse proxy maps pre-body transport errors to 502. Once it has committed
the response headers, a body error aborts the response. Transformed and original
bytes are never concatenated as a recovery strategy. A client can have received
an earlier transformed prefix; this is not transactional whole-document delivery
or an HTML sanitizer. Context cancellation does not make a failed upstream body
readable again, even under fail-open policy.

Route-owned atomic counters record rewritten, bypassed, rejected, and failed
streams. They contain no response content. `rewritten` counts admitted rewrites;
`failed` can subsequently increment for the same stream. These rewrite-specific
counters remain private. Listener [access logs and metrics](observability.md)
record committed status, body-byte counts, aborts, and body I/O errors, including
responses whose handler unwinds with `http.ErrAbortHandler`.

## Experimental eligibility and HTTP behavior

- Full status-200 `GET` responses declaring `text/html` are selected.
  An absent charset, UTF-8, and US-ASCII are supported. Missing content type,
  non-HTML, other methods, and other statuses pass through. An invalid
  non-empty or duplicated content type follows failure policy because eligibility
  is ambiguous.
- HEAD uses the same eligibility and pre-body failure-policy checks as GET.
  Supported HTML HEAD responses omit the origin's length, validators, digests,
  encoding, and range metadata: these do not describe the rewritten GET body.
  HEAD stays HEAD upstream, consumes no content, and allocates no Wasm instance;
  it does not predict subsequent GET capacity or body-time failures. Non-HTML
  HEAD metadata passes through unchanged.
- Request upgrades bypass the wrapper entirely, preserving the duplex response
  body. SSE, gRPC, and 204 are outside HTML transformation.
  Partially transformed representations are unsupported. An unsolicited 206
  (including multipart ranges) or 304 after the adapter requested a full response
  is an upstream protocol error in both modes; no body is consumed or forwarded.
- The experiment accepts identity-encoded HTML. It does not implement an input
  gzip/Brotli decoder; unsupported encodings follow the route's failure policy.
  Existing downstream gzip can compress successfully transformed output.
- For non-upgrade GET/HEAD requests, a cloned request drops read preconditions
  (`If-Match`, `If-Unmodified-Since`, `If-None-Match`, `If-Modified-Since`),
  `Range`, and `If-Range` to obtain a complete representation.
  The caller's request is unchanged. This
  also sacrifices upstream revalidation/range optimization for non-HTML responses
  on that route, because the content type is not known until the response arrives.
  This is deliberately one upstream request per attempt: it performs no probe or
  conditional refetch and generates no rewritten validators. The adapter then
  evaluates the original conditions against the selected representation.
  Rewritten streams have no origin validator/date: a tag-valued If-Match fails
  with 412, tag-valued If-None-Match cannot match, and date conditions are ignored.
  Wildcards still evaluate existence (If-None-Match `*` gives 304). Untouched and
  fail-open responses use their retained metadata. Normal non-2xx outcomes take
  precedence; malformed tag lists give 400. A conditional response consumes no
  body and releases any admitted instance. All ranges are deliberately ignored
  and served as full representations; no partial rewritten response is generated.
  Accept negotiation and every Vary field are retained. Statute's TTL Cache now
  selects Vary variants and delegates conditional/range requests downstream;
  the downstream representation producer evaluates their original conditions.
- Transformed responses discard origin length, ETag, Last-Modified, digest,
  encoding, and Accept-Ranges metadata. Rejection/bypass decisions happen before
  those changes. Request and origin no-transform are respected; strict routes reject that
  selected HTML, and open routes pass it through unchanged with additional
  `Cache-Control: no-store`. Selected responses with trailers or Content-Range
  follow failure policy. Repeated Content-Encoding fields are checked together.
  Undeclared trailers discovered at EOF terminate the stream in either mode.
- Cache-Control parsing combines the policy from every field value, including
  noncanonical map keys supplied by custom transports. Quoted commas and escaped
  quotes in extension arguments do not create directives. Malformed directives
  follow the same explicit failure policy before any HTML body is consumed.
  Non-HTML responses retain their existing passthrough behavior.

The original representation is never retained for rollback. Failure policy only
allows fallback before body consumption starts. The prototype does not attempt
to detect a late parser failure before committing HTTP headers.

## Ownership and limits

`httpEngine` owns shared compiled code and a bounded set of active rewrite bodies.
Each admitted response owns a fresh instance, context, input budget, output
budget, and a pull-driven reader. There is no producer goroutine. Reading one
4-KiB input chunk can produce buffered output up to the remaining output budget;
the downstream drains that output before the next input read. The guest retains
its existing 1-MiB parser and 32-MiB linear-memory limits. These limits do not
constitute a total process RSS bound.

The route timeout begins before the upstream round trip and lasts through body
consumption, including non-HTML/bypassed GET/HEAD responses. Other methods and upgrade
requests bypass that experimental timeout. A known oversized response follows
failure policy before reading; an unknown-length response that crosses the
input budget terminates once consumption has begun.

Body close cancels upstream work, joins an outstanding body read, closes the
instance, and releases admission. Engine close first prevents new admissions,
then cancels and closes active rewrite bodies before closing the runtime. The
caller retains ownership of the underlying transport and HTTP server. Socket
write deadlines and server shutdown still belong to that server: a context
deadline alone cannot interrupt every blocked downstream write.

Tests use four active instances, a 1-MiB input budget, 2-MiB output budget, and a
five-second route timeout, with smaller limits for failure cases. Production
defaults and measured capacity remain undecided.

## What the tests prove

`TestHTTPPolicyIsolationAndMetadata` uses one shared transport/engine with
opposite route policies, checks the original request is unchanged, and verifies
validator removal versus original metadata preservation. `TestHTTPEligibility`
exercises both policies across the selection matrix. `TestHTTPPolicyRequired`
rejects implicit policy and unbounded configuration.

`TestHTTPStreamingAndTerminalFailure` receives rewritten output over a real
HTTP connection while the origin is deliberately held before EOF. A later output
limit error interrupts both policy modes. `TestHTTPDisconnectReleasesInstance`
closes the downstream body and waits for both proxy and origin handlers to exit.
Admission, deadline, engine shutdown, unknown-length input limits, and no
read-ahead without downstream demand have separate tests.
`TestHTTPAmbiguousHeadersAndLateTrailers` checks duplicated content types,
stacked encoding fields, and trailers announced only after body streaming.
`TestHTTPNoTransformPolicy`, `TestHTTPCacheControlGrammar`, and
`FuzzHTTPCacheControl` cover request/response transformation prohibitions,
quoted extension values, malformed fields, and pre-body decisions in both modes.
`TestHTTPStatuteNoTransform` verifies both policies and quoted-extension handling
through a separate Statute process.
`TestHTTPHeadRepresentation` proves metadata removal, no body consumption or
Wasm admission, request cloning, and non-HTML metadata preservation.
`TestHTTPFullRepresentationRequest` checks the outbound request contract and
Vary preservation for HTML and non-HTML GET/HEAD. `TestHTTPUnsolicitedRepresentations`
rejects unsolicited 206/304 across both failure policies and media types.
`TestHTTPStatuteHeadMetadata` checks the client-visible HEAD response through
the real Statute header-writing path in a separate process.
`TestHTTPMultiplexedStreamInterruption` negotiates real HTTP/2 and HTTP/3
connections to the private reverse proxy. In each failure mode, a rewritten
prefix arrives before origin EOF; closing the downstream stream releases both
handlers and Wasm admission. Exceeding the output limit instead resets the
stream and reports a read error, never a clean end or an untransformed suffix.
These protocol tests exposed a production HTTP/3 truncation bug: the standard
reverse proxy suppressed body-copy aborts because quic-go's context lacked the
standard server marker. Statute now supplies its companion HTTP server through
that marker; quic-go handles `http.ErrAbortHandler` by resetting the stream.
`TestHTTP3ProxyBodyError` proves the fix over a real Statute HTTP/3 listener for
both pool proxies and custom `Handle` proxies. Docker generation integration
remains separate work.

`TestHTTPStatuteProtocolInterruption` repeats the rewrite/disconnect/output-limit
matrix through a separate Statute process's actual HTTP/2 and HTTP/3 listeners.
It verifies the negotiated protocol, rewritten output before origin EOF, protocol
stream-reset errors, and origin-request release. A successful subsequent rewrite
under a single-instance admission limit proves that interruption releases the
engine slot. Both route failure policies are covered; the child owns and shuts
down its listeners and engine.

`TestHTTPStatuteAbortedStreamObservation` runs the rewriter in a separate Statute
process, receives a rewritten prefix before the origin connection fails, and
checks the client read error, JSON access record, and Prometheus endpoint. The
committed 200 remains visible with an abort and the emitted prefix's byte count;
page contents never enter the access record. Unit tests cover pre-header and
informational-only aborts, partial writer failures, optimized/fallback body copies,
normal responses, unchanged panic propagation, and filter/sampling precedence.

`TestHTTPStatuteMiddlewareInteractions` launches a separate Statute process and
observes it through HTTP and its readiness/exit logs. The process routes private
reverse proxies through existing `Handle` routes. It uses the real Cache, Retry,
and Compress implementations: cache hits replay one rewritten result, the final
successful retry contains one insertion, and gzip output decompresses to one
rewritten document. An upgrade capability test preserves the bidirectional body.

This experiment transforms each eligible backend attempt. Retry may discard an
already transformed attempt. Cache sits outside the private handler and stores
its result. It does not establish arbitrary middleware-order equivalence or
introduce route policy into Statute's shared pool handlers.

## Resolved integration finding: fail-open and Cache

The initial experiment found that Cache stored a fail-open response despite its
`Cache-Control: no-store`, preventing a later request from attempting rewriting
again. Cache now honors no-store on requests, downstream responses, and the
projected route response headers before storing an entry.

The subprocess test now proves recovery across three requests: the first serves
an unsupported-charset response unchanged with no-store, the second reaches the
origin and receives one rewritten result, and the third replays that rewritten
cache entry. The origin is called exactly twice. This resolves the demonstrated
bypass-retention bug; [other cache limitations](production.md#response-cache)
remain explicit.

`TestHTTPStatuteWarmCacheNoTransform` warms a rewritten cache entry, then proves
that request no-transform still reaches the strict/open decision and cannot
replace the ordinary entry. A quoted extension value containing the same text
does not bypass the cache. `TestHTTPStatuteCacheVaryCompression` selects language
and encoding variants through both Cache/Compress orders, checks decoded rewritten
bytes and encoding headers, and verifies origin call counts across repeated hits.

Root regressions cover all six conditional/range fields, both Cache/Retry orders,
projected Vary, absence versus empty/multivalue request headers, immutable key
snapshots, per-variant expiry, changed Vary schemas, and non-storage of partial or
unselectable responses. The original cache failed these selection/bypass tests.

### Generated validators

Explicit `ETag()` renders a cloned internal GET for HEAD, through the same
inner representation-producing path. The original method/headers stay unchanged;
render-only precondition/range removal prevents short-circuiting before the bytes
exist. It hashes that rendered representation, evaluates the original request,
and suppresses all body delivery to the HEAD client. Listener observation counts
one external HEAD request. Rewriting without ETag remains body-free for HEAD.
Both Cache orders and compression positions have unit and real-process tests.
`With(ETag(), Compress(...))` hashes encoded bytes and emits a strong validator;
`With(Compress(...), ETag())` hashes identity bytes and emits a weak validator for
encoded delivery. HEAD follows the same inner render and returns the same ETag.
Outer compression omits the identity length from HEAD because it is not the
encoded length. Compression emits no encoded body for HEAD or 304.

Compression inside the ETag render defers codec flushes until completion. This
prevents proxy flush scheduling from changing otherwise identical encoded bytes
and therefore GET/HEAD validators. `TestETagCompressionIgnoresFlushBoundaries`
varies flush boundaries deterministically for gzip and Brotli;
`TestCompressionWithoutETagStillFlushes` proves that ordinary streams still
deliver decodable output before their handlers return.

The shared read-condition evaluator implements strong If-Match, weak
If-None-Match, wildcard/list syntax, date conditions, and precedence from
[RFC 9110 section 13.2](https://www.rfc-editor.org/rfc/rfc9110.html#section-13.2).
GET/HEAD validators are computed at the ETag stage's pipeline position; the first
declared middleware is outermost. Rendering is buffered and costs the same work
as that inner GET, including a Wasm instance for rewritten HTML. Streaming routes
without ETag keep the original body-free HEAD path.

## Manual representation headers

The chosen contract is fail-fast: whoever produces the final bytes owns the
metadata describing those bytes. On a rewrite-enabled route, raw
`SetResponseHeader` and `AddResponseHeader` (append) operations cannot inject
`Content-Type`, `Content-Length`, `Content-Encoding`, `ETag`, `Last-Modified`,
`Content-MD5`, `Digest`, `Content-Digest`, `Repr-Digest`, `Accept-Ranges`,
`Content-Range`, `Transfer-Encoding`, or `Trailer`. Removal remains subject to
the final producer's requirements: the shared route validator rejects response
Content-Encoding Set/Add/Remove with enabled compression, including assembled
Docker chains. Removing a validator or length remains allowed.
CSP, cookies, custom headers, and actual compression stages are unaffected.
There is no last-writer-wins exception, even for a conflicting operation followed
by removal. Reordering middleware or adding Retry does not change validation.

The private subprocess configuration gate resolves the configuration and checks
ordinary and fallback routes before calling `Run`. Tests cover both operations,
mixed-case names, order, removal, non-rewriting siblings, rejected startup, and
real rewritten/compressed responses with allowed headers. This gate recognizes
the experiment's direct reverse-proxy actions; it is not a public Statute
validator for arbitrary transformations hidden inside application handlers.
Public rewrite configuration must carry this ownership into canonical Resolve
validation before release, including dynamically generated routes. A generated
validator stage needs its own correct representation semantics; permitting
compression here is separate from the Cache/ETag composition tests above.

`TestHTTPStatuteCompressionPreservesBypass` verifies that downstream compression
preserves fail-open encoded bytes exactly and leaves a no-transform response
unencoded. Root tests cover response/request no-transform, malformed directives,
partial responses, bodyless statuses, and compressor reuse after an abort.
`TestHTTPStatuteCompressedBypassTrailers` exercises HTML bypassed because it
announces trailers: downstream gzip preserves the original HTML and no-store,
removes the now-stale identity digest, and retains the unrelated completion
trailer. Root tests cover gzip/Brotli, announced and late trailers, noncanonical
header keys, buffered composition, and no-transform bypass.
Eligibility also reads custom-transport header keys case-insensitively:
`TestHTTPNoncanonicalHTMLHeaders` proves eligible HTML is transformed, and
`TestHTTPAmbiguousHeadersAndLateTrailers` rejects or bypasses hidden duplicate
media types, encodings, range metadata, and trailer declarations under the
selected route policy.
`TestHTTPStatuteDrainsRewrittenStream` starts shutdown with a rewritten stream
open, observes TCP ingress refusal, then releases the origin. The existing stream
delivers its rewritten tail and the separate Statute process exits successfully.
The ingress probe retries connection resets within its one-second deadline;
only connection refusal establishes closure. `TestPollHTTPListenerClosed`
injects reset/open/refused sequences and checks that persistent resets, an open
listener, and unexpected errors cannot satisfy the shutdown proof.
The shutdown-control scenario owns its HTTP-triggered signal. Parent cleanup
waits for a clean child exit without sending a second signal; the existing
30-second process deadline still bounds a failed shutdown.
If an assertion fails before shutdown is requested, cleanup cancels the child
immediately. `TestHTTPStatuteFailedControlCleanup` verifies that path in a separate
process and requires completion within ten seconds; successful drain tests still require a
clean exit. Request-ID response headers obey the same representation-ownership
checks as raw header injection.

## Remaining gate

The HTTP audit currently has the following evidence. These are private-experiment
proofs; the public API and production go/no-go decision stay open.

| Obligation                                                | Evidence / boundary                                                                                                                                                                                                                           |
| --------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Media type, charset, encoding, status eligibility         | `TestHTTPEligibility`, `TestHTTPNoncanonicalHTMLHeaders`, and `TestHTTPAmbiguousHeadersAndLateTrailers`; explicit route rejection/bypass before transformation                                                                                |
| Full-representation requests and original read conditions | `TestHTTPFullRepresentationRequest`, `TestHTTPUnsolicitedRepresentations`, `TestHTTPSelectedRepresentationConditions`, and `internal/httpprecondition` tests; ranges are deliberately ignored and full responses selected                     |
| HEAD with and without generated validators                | `TestHTTPHeadRepresentation`, `TestHTTPStatuteHeadMetadata`, `TestHTTPStatuteETagRepresentation`; only explicit ETag renders GET internally                                                                                                   |
| Cache selection and no-transform                          | `TestHTTPStatuteWarmCacheNoTransform`, `TestHTTPStatuteCacheVaryCompression`, and `cache_representation_test.go`; both middleware orders and identity/coded variants                                                                          |
| Manual metadata and Retry                                 | `TestHTTPConfigRepresentationHeaders`, `TestHTTPStatuteConflictingHeadersPreventStartup`, `TestHTTPStatuteMiddlewareInteractions`; no duplicate transformation on successful retry                                                            |
| Compression and trailers                                  | `TestHTTPStatuteCompressionPreservesBypass`, `TestHTTPStatuteCompressedBypassTrailers`, `TestCompressRepresentationTrailers`; origin coding bypass, bodyless output, digest removal, and unrelated trailer preservation                       |
| Encoding negotiation and validator stability              | `TestHTTPStatuteCompressionNegotiation`, `TestHTTPStatutePreservesAcceptedOriginCoding`, `TestHTTPStatuteETagRepresentation`, and root negotiation/flush tests; exclusions, quality, variants, 406, and flush-independent GET/HEAD validators |
| Cancellation, timeout, shutdown, and admission release    | `TestHTTPAdmissionCancellationAndShutdown`, `TestHTTPDeadlineAndKnownLength`, `TestHTTPDisconnectReleasesInstance`, `TestHTTPStatuteProtocolInterruption`; HTTP/1, HTTP/2, and HTTP/3 interruption evidence                                   |
| Pull-driven reads and upgrades                            | `TestHTTPPullAndUnknownLengthLimit`, `TestHTTPUpgradePreservesDuplexBody`; bounded read-ahead and duplex-body preservation                                                                                                                    |
| Docker generation and response lifetime                   | `TestHTTPDockerRewriteLifetime`, `TestHTTPDockerRouterPolicyChange`, `TestHTTPDockerLeaseCoversRewrittenResponseEnd`; private hook described below                                                                                            |
| Final outcome observation                                 | `TestHTTPStatuteAbortedStreamObservation` and listener tests; status, bytes, abort/I/O-error signals, filter precedence, and one external HEAD observation                                                                                    |

`Compress` parses Accept-Encoding qualities, repeated fields/codings, and
wildcards. It preserves acceptable origin
encodings and rejects a successful response with an empty 406 when no allowed
representation can be delivered. Malformed preferences produce 400. The precise
selection, duplicate, bodyless-status, and error-response contracts are documented
under [body-derived ETags and compression](production.md#body-derived-etags).
Hosted validation of the complete changes is pending. Public
rewrite-specific instrumentation, real slow-client load budgets, and public
configuration remain separate production gates.

### Private Docker integration

The `htmlrewrite_research` build tag enables a private, internal test seam.
It captures policy per Docker route at generation construction, carries it on
the request inside the existing workload request scope, and applies the private
adapter around the real selected pool transport. Shared pools retain no route
policy. The factory is fixed before startup. Ordinary builds use identity helpers
and contain neither the hook registry nor a Wasm dependency. This adds no public
middleware constructor, resolved field, or label.

`TestHTTPDockerRewriteLifetime` observes a separate Statute process through HTTP
and a deterministic Docker API fixture. A rewritten stream holds its workload
past the idle window; completing it permits idle stop. A replacement immutable
container reaches idle independently while the old stream remains open, and
old-stream completion does not prevent subsequent activation/idle settlement.
Plain and rewritten routes sharing the successor pool retain separate policy.
`TestHTTPDockerRouterPolicyChange` replaces router labels on the same binding:
new requests see the new route policy, the old stream retains its captured policy
through its tail, and the lease remains held until that stream ends.
`TestHTTPDockerLeaseCoversRewrittenResponseEnd` holds the final response EOF
after both the origin and rewriter have finished. Idle stop remains blocked
until the downstream response completes or is canceled. Both completion paths
then permit idle stop.

The seam marks errors introduced by the route response stage separately from
underlying transport failures. `TestResearchRouteFailureDoesNotDemotePool` proves
that rejection leaves shared backend health alone while upstream 503s retain
failure accounting. This tests the private composition and must be retained in
the eventual public design. It does not replace real-Docker crash/recovery tests
or settle the public middleware API.

From `research/htmlrewrite`:

```sh
make test
CGO_ENABLED=0 go test -run '^TestHTTP' -count=5 -timeout=90s .
CGO_ENABLED=0 go test -tags htmlrewrite_research -run '^TestHTTPDocker' -count=5 .
```

The research module uses a local replacement of the root Statute module for
the shared precondition helper and subprocess tests. Ordinary Statute builds acquire no Wasm/Rust
dependency. The guest still builds explicitly with the pinned Rust toolchain.
