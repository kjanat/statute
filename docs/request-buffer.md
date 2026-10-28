# Request buffering

`RequestBuffer` reads the complete request body before calling the next handler.
Use it to keep uploads that exceed the route's limit from reaching an upstream.
`BodyLimit` checks size while streaming. Retry has its own memory budget and can
forward a request as a stream when it cannot buffer it for another attempt.

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

Each route sets its maximum body size and the threshold for writing to disk.
Configure the total memory and disk budgets once on `Config.RequestBuffers`.
One manager enforces these budgets across all listeners, static routes, fallback
routes and Docker routes on that server. Readers from an old Docker configuration
continue to count toward the same budgets after a configuration change. Routes that share an upstream
pool use these same server limits. Each server instance has its own manager.

## Limits and storage

The default is memory-only buffering with a 64 MiB budget. The default memory
threshold is the smaller of 256 KiB and the route maximum. An explicit threshold
must be positive and no larger than the maximum. Writing to disk requires both an
absolute `Storage` path and a positive `DiskBudget`. Incomplete disk configuration
fails resolution. Configuration resolution performs no filesystem operations.

Create the storage directory with permissions suitable for private request data.
Statute creates temporary files with private permissions in that directory and
removes each filename immediately. The open file remains readable until the last
reader finishes. The filesystem must support removing an open file. An unavailable
or unwritable storage directory causes requests that need disk storage to receive
503. Statute leaves the directory and files belonging to other applications alone.

Space is reserved before it is used:

- Requests without a body reserve a buffer slot but no body memory. A known
  positive `Content-Length` sizes the initial allocation, including one extra byte
  to detect an oversized body. The length is a sizing hint: buffering still reads
  through EOF and enforces the route maximum.
- Unknown-length bodies start with at most 512 bytes. Initial allocations are
  capped by the total memory budget and the route maximum plus one byte, or the
  memory threshold plus one byte when disk storage is enabled. A nonempty reader
  with a zero length is treated as unknown, so its bytes and trailers are read.
- Memory grows as needed, up to that cap. Both the old and replacement allocation
  count toward the budget during copying; exhausted growth capacity returns 503
  before invoking the next handler. Allow headroom for growth when sizing budgets.
- With disk storage enabled, crossing the threshold reserves the full route
  maximum from the disk budget before writing. The allocated memory remains
  reserved for copying until the last reader releases the buffer.
- A server allows at most 128 buffers at once. Readers kept open after a timeout or
  handler return still count toward this limit. Each nested `RequestBuffer`
  declaration reserves a separate buffer.
- Exceeding the route maximum returns **413**. Insufficient memory, disk space or
  available buffers returns **503** with `Cache-Control: no-store`. Storage
  operations that fail also return **503**. Requests are rejected immediately;
  they are neither queued nor forwarded as streams.
- Other body read errors return **400**. Cancellation returns **503** when a
  response can still be sent. An outer Timeout keeps its configured response.

These budgets cover the bytes reserved by RequestBuffer. Account separately for
other process memory, transport allocations, filesystem metadata and the kernel
page cache. A route maximum can require more space than the server budget permits, even
when no other request is active. Lint reports that configuration as `RBF001`;
smaller requests can still succeed. This warning compares the capacity needed
for the maximum body with the budgets; it does not predict concurrent use or
the additional capacity needed while growing a buffer.

## Middleware order and replay

RequestBuffer runs at its declared position. Put authentication or IP policy
before it to reject unauthorized requests before reading their uploads. Put
RequestBuffer before Retry to buffer once across retry attempts. Retry keeps its
existing method and protocol restrictions, 1 MiB replay ceiling, memory budget
and streaming fallback. Buffering a POST does not make it eligible for retries.
If Retry comes first, each attempt can call RequestBuffer again, and Retry applies
its configured response-status rules.

The next handler runs only after EOF and final trailer capture. It receives the
measured `ContentLength`, completed trailers, a fresh `Body` and a `GetBody`
function for creating replay readers. Close every reader returned by `GetBody`.
A body with zero bytes and no trailers is forwarded as `http.NoBody`, preserving
Cache's handling of empty HTTP/1 requests. Bodies and trailers still cause Cache
to bypass the request, as do HTTP/3 requests under its existing rules. The native
proxy preserves trailers when forwarding to HTTP/1 and HTTP/2 upstreams.

## Protocols and cancellation

Requests of unknown length have the same size limit. Reading a body with
`Expect: 100-continue` lets the HTTP server send its normal interim response.
A known oversized body or unavailable initial capacity is rejected before reading.
For HTTP/1, rejection also stops the server from waiting for the remaining upload
before sending the response. This covers both early rejection and rejection after
some body bytes have been read.

A bodyless upgrade can proceed after EOF. Upload streams, CONNECT tunnels and
streaming RPCs that never reach EOF cannot reach the next handler through
RequestBuffer. Use separate routes for those protocols. HTTP/2 and HTTP/3 bodies
and trailers follow the same buffering rules.

Cancellation interrupts the body read through the transport's read deadline.
Custom readers must support cancellation or deadlines, or eventually finish their
reads. Deadline calls are synchronized with the outer HTTP handler's return.
After that handler returns, timeout workers cannot start another incoming body
read or use its response writer. Cancellation remains effective while the incoming
body is being closed.

## Lifetime and shutdown

Completion, failure and panic release the handler's use of the buffer. Timeout
workers and native transports keep its memory and file reserved until their
readers finish. Each `GetBody` reader also keeps the buffer available until closed.
Once the buffer has been released, `GetBody` returns an error. Custom handlers that
need the body after returning must obtain a `GetBody` reader and close it afterward.

Shutdown rejects new buffers, allows the normal grace period, then cancels any
remaining body reads when that period expires or shutdown finishes. Open readers
keep their storage reserved and readable until they close, including workers that
outlive shutdown. Temporary filenames have already been removed. Construction and
startup open no temporary files; a failed startup can be retried with the same
server.

## Docker and tooling

Register the route policy by name:

```go
statute.Docker().Middleware("upload-buffer@file",
    statute.RequestBuffer("5MiB").MemoryThreshold("256KiB"),
)
```

Router labels can select `upload-buffer@file`. Configure storage paths and total
budgets once through `statute.Config.RequestBuffers`. All Docker routes use that
server manager, including readers still running from a previous configuration.

Export includes the normalized server limits and each route's buffering policy.
Graph shows the shared resources and policies for static routes, fallback routes
and named Docker middleware. Lint validates the configuration and warns when a
route requires more space than the budgets allow. These tools perform no filesystem
operations.
