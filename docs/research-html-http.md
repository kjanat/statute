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

The reverse proxy maps pre-body transport errors to 502. Once it has committed
the response headers, a body error aborts the response. Transformed and original
bytes are never concatenated as a recovery strategy. A client can have received
an earlier transformed prefix; this is not transactional whole-document delivery
or an HTML sanitizer. Context cancellation does not make a failed upstream body
readable again, even under fail-open policy.

Route-owned atomic counters record rewritten, bypassed, rejected, and failed
streams. They contain no response content. `rewritten` counts admitted rewrites;
`failed` can subsequently increment for the same stream. Production metrics and
access-log integration are still pending.

## Experimental eligibility and HTTP behavior

- Only full status-200 `GET` responses declaring `text/html` are selected.
  An absent charset, UTF-8, and US-ASCII are supported. Missing content type,
  non-HTML, HEAD, other methods, and other statuses pass through. An invalid
  non-empty or duplicated content type follows failure policy because eligibility
  is ambiguous.
- Request upgrades bypass the wrapper entirely, preserving the duplex response
  body. SSE, gRPC, 204, 304, and 206 are outside HTML transformation.
  Partially transformed representations are unsupported.
- The experiment accepts identity-encoded HTML. It does not implement an input
  gzip/Brotli decoder; unsupported encodings follow the route's failure policy.
  Existing downstream gzip can compress successfully transformed output.
- For non-upgrade GET requests, a cloned request drops `If-None-Match`,
  `If-Modified-Since`, `Range`, and `If-Range` to request a complete representation.
  The caller's request is unchanged. Other preconditions remain intact. This
  also sacrifices upstream revalidation/range optimization for non-HTML responses
  on that route, because the content type is not known until the response arrives.
- Transformed responses discard origin length, ETag, Last-Modified, digest,
  encoding, and Accept-Ranges metadata. Rejection/bypass decisions happen before
  those changes. Origin no-transform is respected; strict routes reject that
  selected HTML, and open routes pass it through unchanged with additional
  `Cache-Control: no-store`. Selected responses with trailers or Content-Range
  follow failure policy. Repeated Content-Encoding fields are checked together.
  Undeclared trailers discovered at EOF terminate the stream in either mode.

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
consumption, including non-HTML/bypassed GET responses. Non-GET and upgrade
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

## Integration finding: fail-open and Cache

The current Statute cache stores successful responses without checking
`Cache-Control: no-store`. The subprocess test reproduces this explicitly:
an unsupported-charset response passes under fail-open policy with no-store,
then a second request receives that cached unmodified response even though the
origin would now return supported HTML.

The test records the current cache behavior. Production integration must ensure
bypassed responses do not become reusable transformed cache entries. No production
cache behavior is changed here.

## Remaining gate

The experiment establishes the first HTTP path and explicit per-route failure
choice. It does not complete the whole HTTP checklist: Docker generation and
workload-lifetime interactions, hoisted response-header conflicts, complete
conditional/range/HEAD representation semantics, production observability,
HTTP/2/3 interruption, real slow-client load budgets, and public configuration
remain to be addressed. The fail-open/cache finding must be resolved before that
combination is supported. The public API and production go/no-go decision stay open.

From `research/htmlrewrite`:

```sh
make test
CGO_ENABLED=0 go test -run '^TestHTTP' -count=5 -timeout=90s .
```

The research module uses a local replacement of the root Statute module only for
the subprocess integration tests. Ordinary Statute builds acquire no Wasm/Rust
dependency. The guest still builds explicitly with the pinned Rust toolchain.
