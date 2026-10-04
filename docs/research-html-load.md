# HTML rewriting: larger-document and HTTP load probe

This follow-up to [#114](https://github.com/kjanat/statute/issues/114) measures the
private adapter. It does not add a public API or exclude callbacks, handlers, or
static files from the production design.

## Measurement contract

The research harness owns the load, child process and observations. Production
ownership is unchanged. Each response uses a fresh Wasm instance with cancellation
enabled. All clients and the child process have bounded lifetimes and are joined
on failure as well as success. HTTP errors, truncated bodies, checksum mismatches,
measurement failures and failed post-load serving fail the probe.

The parent owns a loopback origin and concurrent HTTP clients; a separate Statute
process serves passthrough (`plain`), streamed rewriting (`stream`), or ETag-buffered
rewriting (`etag`). An independent native LOL HTML execution supplies the expected
rewritten checksum before measurement. Every measured response is checked.
Origin/client/oracle memory is excluded from server RSS observations.
The origin and clients still share the host's CPU and loopback network with Statute.
The shared fixture constructs the engine even for `plain`: that baseline includes
resident compiled rewrite code. Compilation is outside the latency window in every mode.

One verified request warms the server. A closed-loop workload then admits requests
for the configured duration and drains those already admitted. Throughput uses the
actual elapsed time including drain. Nearest-rank p50/p95/p99 values describe
completed requests; low sample counts are printed and cannot establish tail SLOs.
First-body latency runs from HTTP dispatch until the first actual body read.
Slow clients pace bytes at 16 KiB per configured interval, independently of socket
read fragmentation. Kernel/network buffering permits read-ahead.

Server RSS is sampled every 25 ms. Linux process HWM includes compilation, warm-up
and the entire child lifetime; it is not a sampled peak or a per-response heap
bound. Post-load RSS is reported without forcing server GC. The separate engine
memory probe records Go allocation/heap metrics and post-GC retained memory across
rounds. It uses a discard sink, so its memory is not an ETag/cache budget.

The HTTP fixture uses four admission slots, an 8-MiB input ceiling, 32-MiB output
ceiling, 30-second rewrite deadline and 35-second server write deadline. These
apply only to the research load mode; ordinary correctness fixtures retain their
existing limits. Admission rejection remains an error in the
capacity run. Saturation failure semantics have separate correctness tests.

## Candidate envelope, fixed before measurement

These investigation criteria apply to this Pi's four-worker, warm-code execution.
Production defaults and the issue-wide go/no-go require their own decision:

- Dense/sparse documents of 32,768 and 131,072 fragments (roughly 1.3/5.3 MiB).
- Fast streamed first-body p95 below 100 ms; completion p95 below 2 seconds for
  the smaller document and 8 seconds for the larger one.
- Buffered ETag completion under those same fast-client budgets; its first body
  necessarily waits for complete rendering.
- At 16 ms per 16 KiB (1.024 MB/s, about 0.98 MiB/s), completion below output-size/rate plus
  the corresponding fast-client completion budget.
- Child process HWM below 256 MiB in each HTTP case.
- Three 60-second engine-load rounds at four workers on the smaller dense
  document: last post-GC heap no more than 16 MiB above the first post-GC round.
  This retention bound applies to the tested rounds and corpus.
- Every measured response correct; the child serves correctly after load.

Keep the criteria fixed and report any failed candidate bound. Sparse/dense fixture measurements do not cover
all malformed documents or callback programs. Larger representative corpora,
supported-platform comparisons and production resource accounting remain work.

## Reproduction

From `research/htmlrewrite`:

```sh
make http-load HTTP_LOAD_MODE=stream HTTP_LOAD_SHAPE=dense HTTP_LOAD_COUNT=32768 HTTP_LOAD_WORKERS=4 HTTP_LOAD_DURATION=5s
make http-load HTTP_LOAD_MODE=etag HTTP_LOAD_SHAPE=sparse HTTP_LOAD_COUNT=131072 HTTP_LOAD_WORKERS=4 HTTP_LOAD_PACE=16ms HTTP_LOAD_DURATION=5s
make memory LOAD_DURATION=60s LOAD_ROUNDS=3 LOAD_WORKERS=4 LOAD_COUNT=32768 LOAD_SHAPE=dense
```

Run each HTTP matrix cell in a fresh test process and repeat with `plain`,
`stream`, and `etag`. Build once and invoke `artifact/http-load.test` directly
to avoid rebuilding between cells. Keep the emitted configuration/checksum with
the result. The ordinary suite runs a short correctness smoke case; long runs
remain explicit measurements.

## Pi 5 matrix results (2026-10-04)

[Raw commands and results](../research/htmlrewrite/results/arm64-http-load-2026-10-04.txt)
record all 24 cells: two sizes, two match densities, three response paths, and
fast/paced consumers, with four clients per cell. Go 1.27.1,
Rust nightly-2026-10-03 (1.101.0-nightly, commit 0abfedbc7), Linux arm64,
GOMAXPROCS=4, CGO_ENABLED=0; default guest SHA-256
`598d6ce8f90bde6b20333f91b827db323ab836345afcd7a5ff755aa0a04e81ef`.
These runs were sequential on the development Pi with background workloads;
about 8.1 GiB of host swap was occupied before the run. They are not isolated-host
capacity certification. RSS excludes swapped-out pages; it is not total committed
memory. No cancellation or isolation controls were disabled.

Fast-client p95 times are milliseconds; HWM is server process MiB:

| Input           | Plain total | Stream first body | Stream total | ETag first body | ETag total | Stream / ETag HWM |
| --------------- | ----------: | ----------------: | -----------: | --------------: | ---------: | ----------------: |
| 1.28 MiB dense  |        14.5 |              19.7 |        757.2 |           675.9 |      683.4 |       50.0 / 77.4 |
| 5.13 MiB dense  |        49.2 |              22.5 |       2628.5 |          2616.5 |     2647.6 |      47.7 / 195.2 |
| 1.31 MiB sparse |        15.0 |              20.6 |        307.2 |           266.5 |      270.2 |       51.4 / 67.2 |
| 5.25 MiB sparse |        47.9 |              22.7 |        913.6 |           761.9 |      770.4 |      50.7 / 130.8 |

All matrix cells passed response verification and post-load serving, and met the
candidate timing/HWM criteria above. The larger dense input expands to 11 MiB;
at the configured slow-consumer rate, streamed completion p95 was 11.29 seconds
and ETag completion p95 was 13.70 seconds. Rendering overlaps delivery in the
streamed path; ETag first renders and hashes the entire body.

The slow large-document cells completed only four responses each. Their p95/p99
is the maximum of four observations; larger samples are required to establish a tail SLO. Raw
sample counts and elapsed times must accompany any comparison. The larger dense
ETag high-water mark reached 195.2 MiB versus 47.7 MiB for streaming in the fast
case. A parser-admission cap alone therefore cannot be presented as a total
response-buffer/cache memory budget.

### Longer observations

The same raw-results file includes a 60-second admission window for the larger
dense ETag route with four paced consumers. It completed 20 verified responses
in 67.95 seconds including drain, with 13.70-second completion p95 and a
241.9-MiB process HWM. The child served another verified response after the load.
That fits the candidate ceiling but leaves little headroom for unrelated server
work. Neither this result nor parser admission supplies a total server memory cap.

Three subsequent 60-second engine-only rounds on the 1.28-MiB dense document
completed 406, 408 and 415 rewrites. Each round interrupted and joined four
in-flight rewrites at its deadline. Post-GC heap was 2,890,744 / 2,892,288 /
2,891,936 bytes, with two goroutines at every boundary. The final-minus-first
retention difference was 1,192 bytes, below the 16-MiB candidate bound. Peak
sampled heap was 27.3 MiB and RSS 55.3 MiB; after engine closure and GC the
heap was 664,888 bytes. These are three one-minute rounds with explicit GC at
boundaries; production retention must also be evaluated without forced GC.

The initial local build used Go's default CGO_ENABLED=1. Its
[matrix](../research/htmlrewrite/results/arm64-http-load-cgo1-control-2026-10-04.txt)
and [soak](../research/htmlrewrite/results/arm64-soak-cgo1-control-2026-10-04.txt)
are retained as labelled exploratory controls. All results summarized above were
rerun with CGO_ENABLED=0. The probes now emit Go build settings automatically.

## Outcome and implementation consequence

The candidate envelope passes for these fixtures on this Pi. This supports moving
on to the configurable rewrite-program/host-guest interface and production
ownership design; another generic HTTP compatibility research pass is unnecessary.
It does not certify arbitrary callback programs or every supported platform.

Production resource accounting must include buffered representations and cache
retention separately from live parsers. Preserve cancellation and fresh-instance
isolation. Configure and test all intended response-producing route actions and
Go callbacks without imposing a proxy-only or declarative-only release boundary.
Cross-platform operating budgets, representative malformed/application corpora,
public integration and supported-platform distribution remain explicit #114 work.
