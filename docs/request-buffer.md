# Request buffering

`RequestBuffer` reads a complete, bounded request body before invoking the next
handler. Use it when an upstream must not receive a partial upload that later
exceeds the route's limit. `BodyLimit` remains a streaming cap; Retry retains its
separate replay admission and single-attempt streaming fallback.

```go
statute.Config{
    RequestBuffers: statute.RequestBufferResources{
        MemoryBudget: "64MiB",
        Storage:      "/var/lib/statute/request-buffer",
        DiskBudget:   "4GiB",
    },
    Routes: statute.Routes{
        statute.Match("/upload").ProxyTo("api").With(
            statute.RequestBuffer("5MiB").MemoryThreshold("256KiB"),
        ),
    },
    // Declare listeners and the api upstream as usual.
}
```

The route owns the positive per-request maximum and memory threshold. The server
owns one resource manager shared by every listener, static route, fallback route,
and Docker generation. Replacing a Docker generation does not replenish capacity
still held by its readers. Pools never own or partition this budget. Separate
server instances have separate managers; this is not a cross-process quota.

## Admission and storage

The default resource configuration is memory-only with a 64 MiB budget. The default
threshold is the smaller of 256 KiB and the route maximum. Explicit thresholds
must be positive and no larger than the maximum. Disk spilling requires both an
absolute `Storage` path and a positive `DiskBudget`; incomplete configuration fails
resolution. Resolve and configuration tooling perform no filesystem I/O.

Provision the storage directory yourself with permissions appropriate for private
request data. Statute neither creates the root nor sweeps files belonging to
other applications. Temporary files use private permissions and are unlinked
immediately after creation; open descriptors retain their contents until the final
reader releases them. The filesystem must support unlinking an open file. An
unsupported or unwritable storage root fails admission with 503.

Admission is conservative and does not queue:

- Memory-only requests reserve the route maximum plus one oversize-detection byte
  before reading, including empty and unknown-length requests.
- With disk enabled, requests reserve the threshold plus one byte. Above the
  threshold they additionally reserve the full route maximum from the disk budget.
  The memory reservation remains charged as spool scratch through final release.
- One server admits at most 128 live buffers. This includes retained replay readers
  and late producers, bounding file descriptors and per-buffer bookkeeping as well
  as body storage. Nested RequestBuffer declarations each consume admission.
- Bytes beyond the per-request maximum produce **413**. Exhausted memory, disk,
  or buffer slots, and storage-operation failures produce **503** with
  `Cache-Control: no-store`. There is no streaming fallback.
- Other input read failures produce **400**; cancellation produces **503** if a
  response can still be delivered. An outer Timeout keeps its own deadline response.

Limits account for buffer-owned bytes and reservations, not total process RSS,
transport allocations, filesystem metadata, or the kernel page cache. A maximum
that cannot fit the server reservations can produce 503 even with no other active
request; lint reports this as `RBF001`.

## Ordering, replay, and protocols

The middleware runs at its declared position. Put authentication or IP policy
before it to reject unauthorized requests without first consuming an upload. Put
RequestBuffer before Retry to buffer once across attempts. Retry retains its
idempotency, protocol exclusions, 1 MiB ceiling, and independent budgets; a
replayable body does not enable retries for POST or enlarge Retry's ceiling.
An outer Retry can re-enter RequestBuffer and apply its configured response-status
policy. Nothing is hoisted or reordered.

Downstream invocation starts only after EOF and final trailer capture. The forwarded
request carries the measured ContentLength, completed trailers, a fresh Body and a
GetBody factory. Each GetBody reader must be closed. The native proxy preserves
trailer framing when sending the buffered body to HTTP/1 or HTTP/2 origins.

Unknown Content-Length is supported with the same strict maximum. Reading a body
with `Expect: 100-continue` lets the HTTP server send its normal interim response;
known oversize and unavailable initial capacity are rejected before that read.

There is no protocol bypass. A bodyless upgrade can proceed normally after EOF;
request streams, CONNECT tunnels, and streaming RPCs that never reach EOF cannot
reach their downstream handler through RequestBuffer. Use separate routes for
those protocols. HTTP/2 and HTTP/3 bodies and trailers use the same admission rules.
Cancellation interrupts the ingress read deadline where the transport supports it;
custom readers must honor cancellation/deadlines or eventually finish their reads.

## Lifetime and shutdown

Handler completion, panic and failure release their ownership. Timeout producers
and native transports retain leases until their actual readers finish; an outer
handler returning does not reclaim storage still in use. GetBody readers own
independent leases and cannot reopen retired storage. Custom retention of the
original Body beyond handler lifetime is unsupported; use and close a GetBody reader.

Shutdown stops new admissions, permits the normal grace period, and cancels remaining
buffering work when the grace period expires or shutdown finishes. Temporary names
are already removed. Retained readers remain valid and charged until closed, even
if an application producer outlives shutdown; resources are never reclaimed from a
live reader. Startup acquires no spool files, so failed startup leaves the same
manager available for a later startup attempt.

## Docker and tooling

Register the route policy by name:

```go
statute.Docker().Middleware("upload-buffer@file",
    statute.RequestBuffer("5MiB").MemoryThreshold("256KiB"),
)
```

Router labels can select `upload-buffer@file`. They cannot supply storage paths or
aggregate budgets. Configure `RequestBuffers` once on `statute.Config`; all current
and retired Docker routes borrow that manager.

Export includes normalized server resource limits and route policy. Graph shows
resource ownership and policies for static, fallback and named Docker chains. Lint
validates configuration and reports route reservations that cannot fit the server
budgets. None of these tools creates directories or opens spool files.
