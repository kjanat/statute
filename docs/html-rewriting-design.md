# HTML rewriting: production design proposal

This is the proposed implementation contract for
[#114](https://github.com/kjanat/statute/issues/114), following merged research
through [#147](https://github.com/kjanat/statute/pull/147). It is not an available
public API or a production-readiness claim. Names below are illustrative.
The decisions at the end are accepted for implementation; production availability
still depends on the delivery gates below.

## Outcome and evidence

Proceed to implementation of configurable programs and a callback ABI in the
private engine, then integrate the proven engine into Statute. Another optimizer
comparison is not a prerequisite. Keep cancellation enabled, fresh instances,
and the existing optimizer baseline.

The current guest has two hard-coded selectors; its only Go callback emits
output. The HTTP experiment wraps a transport. Production needs both a
configurable guest and an action-independent response-writer adapter.

The [HTTP evidence](research-html-http.md), [load measurements](research-html-load.md),
and [Binaryen comparison](research-html-binaryen.md) justify this next step. They
do not establish arbitrary callback safety, production memory defaults, all
platforms, or reproducible release packaging. The issue-wide release go/no-go
remains open until those gates have evidence.

## Architecture contract

| Obligation   | Proposed contract                                                                                                                                                                                 |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Owner        | Route middleware owns program selection, eligibility, failure policy, and per-response state. A server run owns engine resources and aggregate admission. Pools retain transport and health only. |
| Invariants   | No silent policy loss; no cross-route mutable state; no untransformed suffix after rewrite failure; original request and listener observations remain intact.                                     |
| Boundaries   | Surface → resolved → runtime/export/graph/lint, middleware ordering, all four route actions, Docker router generations, listener shutdown and observability.                                      |
| Failure      | Explicit route choice for pre-body unsupported responses or unavailable capacity. Configuration errors never become fail-open. After rewriting starts, both policies terminate on failure.        |
| State        | Immutable program definitions and shared compiled code; fresh per-response instance, callback session, budgets, and cancellation. No mutable instance pool.                                       |
| Interactions | Cache, ETag, Compress, Retry, hoisted headers/path rewrites, upgrades, optimized writer paths, static-file conditions, workload activity leases.                                                  |
| Acceptance   | The named boundary scenarios below, with separate-process tests for HTTP/Docker and build-tag/linkage checks.                                                                                     |

No existing architecture boundary changes merely to reuse the research transport.
This proposal changes no runtime behavior by itself.

## Programs and Go callbacks

A program is an ordered, immutable definition of selectors and operations.
Copy slices/maps at configuration boundaries; do not retain caller-mutable
configuration. Support attribute set/remove, escaped-text or explicitly trusted
HTML insertion/replacement, element removal, and selector-associated Go handlers.
Carry element, text-chunk, comment, end-tag, and document-end events through the
same program model; do not advertise whole-text-node callbacks for a streaming
parser. Full Workers compatibility is not promised.

Declarative operations and callbacks compose in registration order. Use the
pinned LOL HTML selector/parser semantics, including overlapping selectors and
removed elements. Native/Wasm
parity tests must fix the observable ordering before API stabilization.
[LOL HTML settings](https://docs.rs/lol_html/3.0.1/lol_html/struct.Settings.html)
provide ordered handler registration; its
[element API](https://docs.rs/lol_html/3.0.1/lol_html/html_content/struct.Element.html)
provides the underlying mutations.

Proposed Go shape (pseudocode):

```text
Program:
  ordered selector rules and declarative operations
  optional NewSession(context, copied request metadata) → Session
Session:
  Handle(event snapshot, mutation builder) → error
  Close() → error
Route:
  Rewrite(program reference, explicit failure policy, positive limits)
```

The factory is called once per admitted rewrite stage in each Retry attempt.
A session is used serially and closed exactly once after successful
creation, including initialization failure afterward. A failed factory owns its
own partial cleanup. Different responses may invoke the factory concurrently.
Consumer closures remain consumer-owned; sharing external state requires their
own synchronization. Go application code executes outside the Wasm sandbox.

Callbacks receive copied request metadata and event data. Their interface excludes
response writers, mutable requests, guest pointers and parser handles. Metadata is the
route's post-path/header-rewrite view; a synthetic ETag render exposes GET while
retaining an explicit original-method field for diagnostics. That field must not
change representation selection if GET/HEAD validator equivalence is desired.
Never expose page contents through default logs.

An event snapshot can safely be retained as Go-owned data. Its mutation builder
is valid only during that invocation; after return, retained builders reject
writes. Commands execute in recorded order before parsing resumes. A subsequent
handler sees the updated event state. No concurrent mutation, nested feed/finish,
or re-entry into the active instance is permitted. Callbacks must not recursively
dispatch into the same rewrite route and wait for admission.

Return errors, panics, invalid commands, or exhausted command budgets terminate
the rewrite. Do not enable LOL HTML's graceful raw-suffix recovery. Text insertion
escapes text; trusted HTML insertion requires a distinct explicit operation.
Neither is an XSS-sanitization promise: URL values and inserted HTML still need
application policy.

### Event identity and completion

**End-token ownership decision:** a native characterization of pinned LOL HTML 3.0.1
found that `<div><span>text</div>` invokes the span end handler with `</div>`,
then the div end handler with the same token. Removing the end tag from the span
handler removes the ancestor's closing tag. EOF without either closing tag emits
neither callback. The tests in
[`guest/tests/event_contract.rs`](../research/htmlrewrite/guest/tests/event_contract.rs)
cover these cases, explicit/void/same-name nesting and stray ends at three feed
sizes. Run `cargo test --manifest-path guest/Cargo.toml --locked --test event_contract`
from `research/htmlrewrite`.

Statute preserves the accepted missing-end contract. Its pinned local matcher
patch identifies the stack entry actually closed by the token, invokes only that
entry's end handler, and immediately drops implicitly retired descendants'
handlers. Their text/comment scopes and removal counters still retire normally.
Neither user callbacks nor automatic end-tag mutations for an implicit child may
alter its ancestor's token. Source stack identity remains stable across output
tag renaming.

The [source and local-delta notes](../research/htmlrewrite/guest/vendor/README.md)
apply equally to native and Wasm builds. Upstream characterization stays separate
from the patched ownership tests and native/Wasm expected-output regression.
Renovate flags releases for maintainer approval; the documented `update-lol-html`
command reapplies the patch and refreshes the copied source, pins and lockfile. Existing
research tests validate each update. Consumers acquire no additional build step.
This resolves the end-token decision; configurable programs, selector-scope event
identity and the bounded Go callback ABI remain stage 1 implementation work.

The ABI carries a stream ID (unique within the run), rule/handler ID, event kind,
token ID and monotonically increasing invocation ID. Element-start events also
allocate an element ID; all handlers observing that same parsed element share
it. An explicit end-tag event carries that element ID and its own token ID.
Separate stages and retry attempts use separate stream IDs. A mutation reply
must match the full active invocation identity; expired or foreign replies fail.

Text and comment events carry the matching selector-scope element IDs and the
registered handler ID. Nested matches preserve distinct scope IDs; overlapping
rules remain distinguishable without duplicating a handler invocation beyond
the pinned engine's dispatch semantics. Obtain scope membership from the pinned
matcher at dispatch time. Its public text callback alone does not supply this
correlation: the guest needs a narrow matcher-context hook, shared by the native
oracle. Stage 1 must prove that hook across implicit closure and malformed input
before freezing the ABI. Do not infer membership from a stack of observed end
callbacks or expose an invented DOM ancestry.

Text events carry a text-unit ID, chunk index, text context/type and the engine's
last-chunk flag. Their copied text retains entity references, which may span
chunks; script/style text keeps its distinct context. No implicit decoding occurs.
The final chunk can be empty; it completes that text unit only. Feed boundaries
do not promise chunk boundaries. Document-end runs once on successful parser EOF;
it is absent on abort. Session.Close runs after either outcome and owns release
of all remaining application state, including unmatched element scopes.

Statute enforces the documented [end-tag contract](https://docs.rs/lol_html/3.0.1/lol_html/html_content/struct.Element.html#method.on_end_tag)
with the matcher patch above: it omits implicitly closed elements and cannot
attach end handlers to void elements.
There is no balanced start/end guarantee and no synthetic end event. Identifiers
remain valid for correlating retained snapshots but never authorize later mutation.

| Event                 | Allowed mutations                                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| Element start         | Attribute set/remove; before/after, replace/remove; prepend/append/set inner content when the element can contain content |
| Text chunk or comment | Before/after, replace/remove the current unit; no ancestor or attribute mutation                                          |
| Explicit end tag      | Before/after, remove the current end tag; no mutation of an already-emitted start tag                                     |
| Document end          | Append content                                                                                                            |

Invalid event/operation combinations fail explicitly. Attribute snapshots retain
the pinned engine's entity-bearing value representation; they are not
entity-decoded. Attribute writes use its set_attribute semantics (quotes escaped,
ampersands preserved). Name these values accordingly in the public API. Text
insertion remains a separate escaped-text operation. Native/Wasm tests include
entity round trips, void/implicit closures, nested overlapping matches and text
split across feeds.

### Host/guest protocol

Use a versioned, length-delimited program/event/command format with bounded
counts and lengths. Selectors and callback IDs are data, never exported symbol
names or addresses. Validate ABI version, imports, exports, integer arithmetic,
memory ranges, opcodes, event kinds, and instance-local sequence IDs.

While a Rust handler holds an element borrow, it invokes one synchronous host
callback. Go copies the event, runs the session, and writes a bounded command
reply into a guest-provided, capacity-checked region. Rust applies the reply
after the host call returns. Go does not recursively call an exported Wasm
mutation function while Rust holds that borrow. Oversized replies fail. Prove this exchange with the pinned
runtime before freezing its wire format.

Input, event, command, and output bytes have explicit copy ownership; no slice
backed by guest memory escapes a host call. Include protocol and limits in
artifact compatibility checks. Unknown protocol versions fail initialization.

### Cancellation is cooperative for Go code

Retain wazero's active-call cancellation. Arbitrary Go callbacks can still block,
allocate, or perform application I/O. A callback
must observe its context and return; its cleanup must also be bounded. Invoke it
synchronously with backpressure, never in a detached goroutine to manufacture
an apparent timeout. Do not release admission while callback work still exists.

Shutdown is drain-first: admitted sessions retain their contexts during grace.
At grace expiry, cancel remaining sessions and force-close their network streams.
The run retains unfinished factory, Handle and Close invocations through the
eventual-close protocol below. Returning a shutdown timeout does not release
their ownership or destroy their in-use engine.

## Configuration and build boundary

Keep `statute_htmlrewrite` as the opt-in implementation tag. Untagged binaries
link neither wazero nor the guest. Configuration types remain visible so Resolve
can report an actionable unavailable-feature error.
`htmlrewrite_research` is not a production enablement tag.
Ordinary consumers build without Rust, wasm-opt, network fetches, or subprocesses.

The resolved model carries immutable ordered rules, program identity, callback
presence, explicit failure policy, and limits. Callback references are opaque
code-owned values, excluded from serialization like existing handler references;
export retains their identity/requirement. Importing such an export without the
required registry binding fails closed.
Do not fingerprint closures by pointer or claim their captures are immutable.

Resolve performs shape, bounds, references, build availability, header-conflict,
and composition validation offline. Selector validation uses the same pinned
parser before any content listener serves. If exact selector validation needs
the guest, it is a startup prerequisite. Diagnostics identify the failing
program/rule and exclude page contents.

External configuration and Docker middleware names select code-owned programs;
they cannot provide callbacks, module paths, or arbitrary Wasm. The assembled
Docker chain runs the same validation before publication. Missing bindings or
conflicts retain the existing router refusal envelope. Sibling routers sharing
the service are unaffected.

Export, graph and lint show program identity, callback requirements, limits,
failure policy and middleware position. Duplicate names and ambiguous bindings
are errors. Callback implementations are fixed for a server configuration;
replacement requires a new explicit identity/revision.

## One adapter for every route action

Implement a streaming response-writer adapter at the declared middleware
position. Leave `poolHandler.transport` unchanged. The adapter wraps proxy,
static-file, handler,
and redirect actions in ordinary and fallback routes. Redirects retain their
normal non-200 eligibility outcome; supporting an action does not mean rewriting
every status it can produce. Listener redirects, ACME and diagnostics are not
route actions and stay outside this feature.

The adapter defers final header commitment long enough to select eligibility and
admit a stream. It transforms synchronous writes without a producer goroutine;
`ReadFrom` must pass through the same transform. Preserve optional capabilities
only when safe. `Unwrap`/ResponseController paths must not offer a body-write
bypass. Flush emits available transformed output, never incomplete raw tokens;
it does not mean the parser can force output from an unfinished HTML token.
Reject selected HTML with trailers before commitment and detect late trailers.

Port the experiment's GET/HEAD, full-representation request cloning, precondition,
range, media type, charset, encoding, no-transform and metadata behavior exactly
before expanding eligibility. Missing Content-Type currently bypasses rewriting;
document and test implicit sniffing for a custom handler. Applications needing mandatory
transformation must explicitly emit eligible HTML metadata and select fail-closed.
Identity-only input follows the current implementation limit and explicit
reject/bypass behavior for every action.

Static files receive an unconditional/range-free cloned request. Evaluate read
conditions against the resulting selected representation. HEAD without
ETag reads no body and admits no instance; outer ETag renders GET internally and
suppresses the final body. Origin metadata is removed only for transformed
representations; bypass preserves it and adds no-store. Unsolicited 206/304 after
request normalization remains an error. Rewrite failures use the terminal outcome
protocol below: an undelivered failed candidate becomes 502; a failure after
actual client commitment aborts the stream.

## Response execution and terminal outcomes

A route execution scope wraps the complete chain, including hoisted response
headers and buffering middleware. It stays inside listener observation and works
for every route action. Transformation remains at its declared position. The
scope tracks actual downstream commitment and owns identified attempt/stage
children, output reservations and a monotonic terminal outcome for each candidate.
Nested Retry introduces child attempts; it cannot clear an ancestor's failure.

| Resource                                  | Acquire / release contract                                                                                                                                                                    |
| ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Concurrent-response slot                  | Acquire once at first rewrite admission per external route execution; all its stages share it. Release after delivery/abort and all unfinished children have settled.                         |
| Instance reservation and engine-use lease | Acquire before factory/guest work for a stage; release after guest execution and Session.Close finish, even when its successful output remains buffered.                                      |
| Retained-byte reservation                 | Charge every owned response-buffer allocation before growth; transfer ownership with selected output; release on discard or final delivery. Copies need their own charge while both are live. |
| Persistent cache allocation               | Transfer/copy under the cache's separate retained-byte budget on successful storage; eviction/expiry owns release. No request or engine lease is held by a stored entry.                      |

Response and instance limits have distinct names and units. Atomically reserve
the structurally possible simultaneous stage instances for the candidate before
entering its first factory, with no admission wait while holding partial slots.
A route needing two stages consumes one response slot and two instance slots.
Reject configuration whose instance cap cannot accommodate its stage topology;
temporary contention follows the route's pre-body policy. Count nested stages
and Retry branches in that topology. Sequential discarded attempts release their
reservations before replacements acquire them. Buffer budgets remain separate.

Buffers participate explicitly: begin a candidate, run its child scope, then
either discard or transfer selected output to the enclosing owner. Discard
cancels and closes that attempt's sessions exactly once, drops its bytes and
releases reservations. A stuck cleanup remains run-owned and prevents starting
a replacement that relies on its capacity. Transfer invalidates the old owner's
release token; panic/cancellation unwinding settles only still-owned tokens.
Returning from Rewrite alone never releases an outer ETag delivery reservation.

Track three milestones: producer header selection, candidate-buffer header
selection, and actual final header/body commitment to the client. Buffered 200
and Flush on responseBuffer do not advance the last milestone. Informational
responses do not commit the final response. The outer scope observes the actual
writer; real streaming Flush may commit it.

Rewrite failures set a typed candidate outcome before unwinding producer code.
The response buffer, Cache, ETag and Retry must consult that outcome before
hashing, storing, choosing status or replaying. Normal handler return cannot
clear it. Failed candidates are never stored, hashed as success or replayed.
Do not append http.Error to a buffer that already selected 200. Before client
commitment, discard the entire failed candidate and create a fresh empty 502
response with stale representation metadata removed. After client commitment,
abort using the existing protocol-specific path; emit no error document or raw
suffix. Fail-open applies only to the pre-body eligibility/admission decision.

Retry keeps its existing method, request-body and attempt-count restrictions.
Ordinary upstream status failures retain configured status-based retries.
Only pre-body transient capacity/initialization failures explicitly classified
retryable may use the synthesized 502 with an opted-in Retry status. Callback
errors/panics, invalid commands, malformed protocol, resource-limit exhaustion,
and all failures after rewriting begins are non-retryable. Cancellation is
non-retryable. Every retry requires no client commitment and successful disposal
of the discarded attempt; it creates fresh stage sessions. Nested retry owners
consume outcomes only from their own candidate. This requires changes to the
current status-only Retry/responseBuffer protocol in the adapter implementation.

## Middleware composition and caching

Preserve declaration order; do not silently hoist rewriting or claim all orders
are equivalent. Multiple rewrite middleware entries are intentional ordered
transforms, with separate sessions and budgets. The Cache rows describe
cacheable declarative programs; callback programs follow the restriction below.

| Outer → inner               | Meaning to preserve and test                                                                                                                   |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Cache → Rewrite → action    | Store final rewritten output; a hit does not invoke callbacks. Fail-open/no-store cannot poison recovery.                                      |
| Rewrite → Cache → action    | Cache original output; rewrite each hit and discard the cached origin's validators/length.                                                     |
| ETag → Rewrite → action     | Hash rewritten bytes; HEAD renders the same GET representation, with one external observation.                                                 |
| Rewrite → ETag → action     | Inner ETag describes original bytes and must not escape as a rewritten validator. Conditional evaluation cannot short-circuit the full render. |
| Compress → Rewrite → action | Rewrite identity bytes, then negotiate downstream encoding.                                                                                    |
| Rewrite → Compress → action | An encoded inner response follows the explicit unsupported-input policy; no implicit decompression or reordering.                              |
| Retry → Rewrite → action    | Each attempt owns a fresh session; discarded attempts release all state. Callbacks may execute more than once.                                 |
| Rewrite → Retry → action    | Transform the representation emitted by Retry; never combine discarded-attempt bytes with final bytes.                                         |

Callbacks can depend on cookies, authorization, request headers or mutable
application state. `Vary` alone cannot describe arbitrary closure dependencies.
Proposed safe initial contract: callback-bearing routes are non-cacheable in
Statute's Cache in either order, and their responses carry no-store. Enforce
admission through immutable route policy as well as output headers. An inner
cache must see that policy before its storage decision. Do not erase an origin
storage prohibition. A future explicit cache-dependency contract can safely
relax this; it is not implied by adding a Vary field.

The same no-store obligation is enforced at final wire commitment. The outer
execution scope carries an immutable callback-policy prohibition, plus any
fail-open bypass prohibition recorded by a child. After hoisted response-header
operations, the final writer restores no-store if necessary on every selected
response, including HEAD, errors and conditional responses. This is an explicit
final enforcement rule: raw Cache-Control operations cannot opt a callback route
back into caching. Preserve compatible directives and origin storage prohibitions;
discard conflicting public/max-age/s-maxage directives when enforcing no-store.
Cache uses the same obligation before lookup/storage in either middleware order.
Export/lint/docs must expose this precedence. Assembled Docker chains receive the
same policy marker before publication, with no sibling-router propagation.

Set/Add/Remove combinations are allowed under that final enforcement rule;
unrelated headers remain untouched. Test case-insensitive/repeated fields and
operations that erase the intermediate map. A real downstream caching proxy
must observe no-store and never reuse Alice's callback output for Bob. Internal
cache non-storage alone does not establish this wire invariant.

Declarative body-only programs can compose with Cache normally. If a later program
allows request-dependent declarative substitutions, those need the same dependency
analysis. A program revision must replace its route cache; Docker identity must
include program/policy semantics without unioning sibling router policy.

## Engine ownership and operating limits

Create a run-owned engine only when configured static/fallback or registered
Docker rewrite policies require it. Engine compilation and selector preparation
belong to `startPrerequisites` before Docker initial publication/content serving.
Record acquisition immediately on `startAttempt`; transfer to `serverRun` at
commit. Rollback closes all acquired state; retry creates a fresh run. Compiled
handler references must resolve only their current run, never a closed predecessor.

One run can share compiled code; the execution scope separates engine-use leases
from delivery and cache allocations as specified above. Generation retirement prevents new selection of old route
policy without cancelling an already admitted response. Docker workload activity
must last through final downstream delivery, even if parser finish happened
earlier. Cache hits must not acquire a spurious Docker workload lease.

Shutdown first prevents new external work and drains listeners while existing
streams can use the engine. On grace expiry, cancel sessions and terminate their
network streams. Close the runtime only after its users exit. Coordinate with
Docker's quiesce/drain/stop sequence and never wait while holding an engine or
provider lock needed by callback completion.

The serverRun retains a retiring rewrite-run object with execution/child
registries, an exactly-once runtime finalizer, accumulated cleanup errors and a
completion signal. Register each accepted route execution before entering its
chain, including executions that have not reached a rewrite stage yet. During
grace those executions may acquire their remaining stage leases. At deadline,
seal stage admission and cancel; on successful drain, seal after executions exit.
Finalization requires sealed admission and zero executions and children.

The stage owner registers a child before invoking NewSession and defers its
cleanup/deregistration through the whole session lifetime. The final exiting
execution/child invokes the runtime finalizer and signals completion when the
conditions hold. The retirement path handles an already-empty run. No background
goroutine is launched merely to abandon a blocked callback.

NewSession returning a session after cancellation still triggers its Close once,
without starting guest work. A factory error/panic must clean its own unreturned
application allocations; Statute releases all resources it acquired around it.
Recover application panics at factory/Handle/Close boundaries, preserve the
terminal failure, and execute remaining Statute cleanup. A Close error or panic
is recorded; Close is never retried. A stalled Close retains its child until it
returns. Guest/module release follows completion of all code using that instance.

Shutdown timeout returns an unfinished-work error while the server continues to
retain the retiring run. After expiry it admits no new stage or external work.
Repeated Shutdown observes the
same retirement and completion signal, can wait within a new caller's grace
window, and never invokes cancellation, session cleanup or finalization twice.
Completion exposes final cleanup errors through that observation; bounded
operational state reports unfinished child count and eventual completion without
callback data. Listener force-close is independent of callback completion.
Neither the serverRun mutex nor the rewrite registry lock is held while waiting.
The last child's deferred cleanup remains responsible even if Shutdown is never
called again. Process termination is the only hard boundary for Go code that
never returns. Failed startup has no response callbacks and closes its engine
synchronously through startAttempt rollback.

### Required ownership and failure regressions

- Hold outer ETag delivery after Rewrite returns: instance leases can be released,
  but buffer bytes and the response slot remain reserved until delivery settles.
- Discard a rewritten Retry candidate: close its session and release its bytes
  before the replacement; test panic and cancellation at each transfer boundary.
- Run two rewrite stages with one response slot and sufficient instance capacity;
  reject an impossible instance cap during configuration without self-starvation.
- Fail callbacks/output before output, after a buffered prefix and after an actual
  downstream flush, across Cache/ETag/Retry orders and both failure policies.
  Assert fresh 502 or stream abort, zero failed-cache entries and zero mixed bytes.
- During shutdown, a cooperative callback completes within grace. In killable
  subprocesses, independently stall factory, Handle and Close past grace: terminate
  client connections, retain reservations, then release the stall and observe one
  finalization and settled unfinished-work reporting, including repeated Shutdown.
- Exercise nested/overlapping event scopes, omitted closing tags, void elements,
  text chunk completion and entity-bearing attributes against the native oracle.
- Exercise both Cache orders and final Cache-Control Set/Add/Remove through a
  real downstream caching proxy with distinct user responses.

Require positive input/output bytes, parser memory, linear-memory pages,
program size, event/command bytes and counts, concurrent responses, and deadline
limits. Parser/output budgets do not bound allocations made by arbitrary Go
callbacks, ETag buffers, cache entries, or downstream write stalls. Account for
each separately in production budgets; use server write/shutdown deadlines for
network stalls. No silent interpreter fallback or unlimited admission queue.

## Delivery sequence and acceptance gates

Deliver these stages as separate PRs. Check a stage only after its implementation
and evidence exist.

1. **Configurable private engine and callback ABI.** Replace fixed rules with
   bounded programs, native/Wasm parity, copied events, command replies and
   per-response sessions. Test overlapping selectors, text chunks, insertion
   escaping, retained/foreign builders, malformed ABI messages, callback errors
   and panics, context cancellation, cleanup failure, and concurrent isolation.
   Reproduce uncooperative callbacks in a killable subprocess; prove the library
   does not falsely release their admission or destroy live state.
2. **Action-independent private adapter.** Prove writer semantics on proxy,
   handler, static files and redirects before adding public declarations. Cover
   implicit Content-Type, empty responses, informational statuses, Flush,
   ReadFrom, upgrades, trailers and HTTP/1.1/2/3 abort behavior. Execute every
   composition row above; retain the exact bypass → rewrite → cached-rewrite
   recovery regression. Add personalized callback/cache isolation tests.
3. **Production integration.** Add surface/resolved types, shared validators,
   runtime assembly, export/graph/lint and Docker registry selection together.
   Cover ordinary/fallback routes, shared-pool opposite policies, unavailable
   tag/registry binding, header conflicts including RequestID, and program
   revision identity. Prove startup rollback then successful serving, repeated
   cleanup, generation retirement and Docker activity through final delivery.
4. **Release qualification.** Package the audited embedded artifact, licenses,
   sources/notices and reproducible build recipe. Validate the explicitly promised
   OS/architecture matrix, tag combinations and CGO_ENABLED=0 consumer builds.
   Measure realistic callback/malformed-document loads plus streamed, ETag and
   cache retention against published operating budgets. Record release go/no-go
   and publish consumer/operations documentation before marking #114 complete.

For code stages run root tests, race on a supported runner, formatter, lint,
lifecycle lint/audit, affected research/native parity suites and build-tag tests.
Run `make test-e2e-regression` for process/container changes. Keep actual test
names and CI evidence with each delivered stage.

## Accepted implementation decisions

1. **Trusted synchronous callbacks:** accept cooperative cancellation and the
   explicit unfinished-shutdown behavior for non-cooperating Go code. Alternative:
   hard isolation requires a different execution boundary.
2. **Declared position and common response adapter:** preserve meaningful order
   differences above, with the execution/attempt ownership and terminal-outcome
   protocol defining client-precommit 502 versus stream abort. Transformation
   stays at its declared position; its lifetime can extend beyond that wrapper.
3. **Callback cache safety:** start with no-store for callback-bearing routes in
   both cache orders, with explicit final wire enforcement after raw header
   operations. Relax only with an explicit, tested dependency/identity contract;
   do not cache personalized callbacks by assuming they are pure.

Recommended choices are the three proposals above. Exact exported API spelling
can follow the private implementation evidence. Supported production platforms,
numerical defaults and release reproducibility remain release-qualification
decisions. They do not block the first implementation stage.
