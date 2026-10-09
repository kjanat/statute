# Request identity

`RequestID()` selects one identifier per invocation, writes it into the configured
request header and request context, and publishes it to the listener access log.
`From(name)` adopts a nonempty inbound value verbatim; otherwise a random ID is
generated. `Header(name)` selects the output field in **both** directions.
An empty output name retains the default `X-Request-Id`. Header names must be
valid HTTP field-name tokens and are normalized during resolution.

Configure at most one RequestID per assembled route, before every enabled Cache.
Retry outside RequestID invokes it per attempt; the final attempt supplies the
returned ID. Put RequestID outside Retry to retain one ID across attempts.

RequestID changes a private downstream header copy, preserving caller headers.
When Retry wraps RequestID, each attempt reads the original inbound `From` value;
an absent value generates a fresh ID even when `From` names the output header.
The request body and live trailers remain shared normally.

Access logging reads a synchronized snapshot of the latest published ID at
handler exit. It does not wait for a timed-out producer. If RequestID has not
run yet, that record has no generated ID; a later publication cannot change it.
Place RequestID before Timeout when timeout records must include its ID.

## Response ownership

RequestID restores its selected response ID at final header commitment, including
cache hits and buffered ETag/Retry responses. Origin replacements, appended values,
and differently cased field aliases cannot replace or accompany the current ID.
The owned ID is excluded from response trailers, including late trailer fields;
unrelated trailers are preserved. Buffered responses retain the effective ID
across late producer writes before replay.
Normal empty responses also receive the selected ID. Explicit route response-header
Set/Add/Remove operations retain their outer precedence and may deliberately
override it. A directly written hijacked upgrade handshake remains owned by the
connection; RequestID does not rewrite handshake bytes.

## Reserved output headers

`requestIDForbiddenOutputHeaders` is the named set below. Rejection is
case-insensitive and unconditional, with or without `From`, Cache, Compress,
or any other middleware. These fields control protocol behavior.
The restriction applies to `Header(...)`;
`From(...)` can read any syntactically valid source field.

### Framing and connection

`Content-Length`, `Transfer-Encoding`, `Trailer`, `Connection`, `Upgrade`,
`TE`, `Keep-Alive`, `Proxy-Connection`, `Expect`, `Max-Forwards`,
`Sec-WebSocket-Accept`, `Sec-WebSocket-Key`, `Sec-WebSocket-Version`,
`Sec-WebSocket-Extensions`, `Sec-WebSocket-Protocol`.

### Representation

`Content-Encoding`, `Content-Type`, `Content-Range`, `Content-Disposition`,
`Content-Language`, `Content-Location`, `Accept`, `Accept-Encoding`,
`Accept-Language`, `Accept-Charset`, `Accept-Ranges`, `Range`, `Content-MD5`,
`Digest`, `Content-Digest`, `Repr-Digest`, `Want-Digest`, `Want-Content-Digest`,
`Want-Repr-Digest`.

### Validators

`ETag`, `Last-Modified`, `If-Match`, `If-None-Match`, `If-Modified-Since`,
`If-Unmodified-Since`, `If-Range`.

### Caching

`Cache-Control`, `Vary`, `Expires`, `Age`, `Date`, `Pragma`, `Warning`,
`CDN-Cache-Control`, `Surrogate-Control`.

### State and security

`Set-Cookie`, `Set-Cookie2`, `Location`, `Refresh`, `Retry-After`,
`Strict-Transport-Security`, `Content-Security-Policy`,
`Content-Security-Policy-Report-Only`, `WWW-Authenticate`, `Proxy-Authenticate`,
`Proxy-Authorization`, `Authentication-Info`, `Proxy-Authentication-Info`,
`Alt-Svc`, `Alt-Used`, `Clear-Site-Data`, `X-Content-Type-Options`, `X-Frame-Options`,
`X-XSS-Protection`, `Referrer-Policy`, `Permissions-Policy`,
`Cross-Origin-Opener-Policy`, `Cross-Origin-Opener-Policy-Report-Only`,
`Cross-Origin-Embedder-Policy`, `Cross-Origin-Embedder-Policy-Report-Only`,
`Cross-Origin-Resource-Policy`, `Origin`, `Access-Control-Allow-Origin`,
`Access-Control-Allow-Credentials`, `Access-Control-Allow-Headers`,
`Access-Control-Allow-Methods`, `Access-Control-Expose-Headers`,
`Access-Control-Max-Age`, `Access-Control-Allow-Private-Network`,
`Access-Control-Request-Headers`, `Access-Control-Request-Method`,
`Access-Control-Request-Private-Network`, `Host`, `Forwarded`, `X-Forwarded-For`,
`X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Forwarded-Port`, `X-Real-IP`.

### Credential-writer exceptions

`Authorization` and `Cookie` remain supported deliberately. Configuring either
as RequestID output disables every Cache on that route, including across request
clones. The existing RequestID-before-Cache ordering rule still applies.
Their selected values are also emitted on the response and logged as request IDs;
use these options only when that disclosure is intended. This mapping provides
no credential verification; authentication remains the responsibility of the
appropriate policy or backend.

With BasicAuth later in the chain, an Authorization-writing RequestID may only
read its From value without a raw request-header Set/Add/Remove operation on
that input. Such operations run before the entire chain and could supply accepted
credentials for an unauthenticated client. Resolve rejects this combination in
any declaration order, including final Docker chain assembly. A credential mapper
after the last BasicAuth remains supported; ordinary client-provided credential
mapping remains supported before authentication.

Other custom identity/tracing fields remain configurable. This named set does
not infer semantics of arbitrary application-specific headers: assigning an ID
to a custom authorization or routing field remains an explicit application policy.

## Docker and migration

Static and fallback routes reject reserved names during Resolve. Invalid Docker
defaults or named registrations also fail Resolve, even when unreferenced.
The final Docker assembly boundary repeats the check; an invalid resolved chain
is refused through the affected router's tombstone while valid siblings remain
available. It never silently drops only RequestID and serves the route.

Replace prohibited `Header(...)` values with an opaque tracing header such as
`X-Request-Id` or `X-Trace-Id`. Configure CORS, compression, response policy, and
other HTTP behavior through their corresponding features. Raw response-header
operations keep their existing validation and precedence.
