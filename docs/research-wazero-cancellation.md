# Wasm cancellation checkpoint research

This follow-up to the [HTML execution investigation](research-html-execution-memory.md)
isolates wazero termination-check overhead in a parser-independent reproducer.
It is research for [#114](https://github.com/kjanat/statute/issues/114).
No production optimization or HTML-rewriting API is introduced. Cancellation, resource limits,
and fresh-instance isolation in the HTML engine are unchanged.

## Reproducer and evidence

The [test-only package](../research/htmlrewrite/cancelprobe/README.md) generates
Wasm directly in Go. It has one notification import, no memory or WASI, and a
xorshift32 loop with 1, 16, or 256 inline steps per iteration. Each benchmark
performs the same 65,536 steps and checks its output against a Go reference.
Compilation and instantiation are outside the timed region.

All measurements below are from one Raspberry Pi 5, Linux arm64, Go 1.27.1,
wazero v1.12.0 compiler engine, on 2026-10-02. The
[raw test and benchmark output](../research/htmlrewrite/results/arm64-cancellation-2026-10-02.txt)
includes commands. The research CI repeats the probe on amd64 and arm64 and
runs race detection on amd64; its logs are the source for those separate hosts.

## Warm execution cost

Median of three one-second samples, `GOMAXPROCS=4`:

| Steps per loop | Checks enabled | Checks disabled, finite control | Enabled / disabled | Module bytes |
| -------------- | -------------: | ------------------------------: | -----------------: | -----------: |
| 1              |       3.147 ms |                        0.111 ms |              28.2x |          218 |
| 16             |       0.236 ms |                        0.114 ms |              2.08x |        1,570 |
| 256            |       0.124 ms |                        0.113 ms |              1.09x |       23,171 |

The overhead exists without HTML parsing, Rust callbacks, allocations inside
the guest, or memory growth. It falls sharply as work per Wasm loop increases.
Within each row the cancellation setting is the only configuration difference.
Between rows, unrolling also changes loop bookkeeping, code size, and compiler
opportunities. Sample order was sequential. These results
are not a predicted speedup for LOL HTML or a precise cost per checkpoint.

The pinned compiler [inserts a termination call at each Wasm loop header](https://github.com/wazero/wazero/blob/v1.12.0/internal/engine/wazevo/frontend/lower.go#L1326-L1358).
Its [dispatcher deliberately returns to Go](https://github.com/wazero/wazero/blob/v1.12.0/internal/engine/wazevo/call_engine.go#L473-L482)
to check closure and allow scheduling: native guest code cannot otherwise be
preempted there. This is consistent with the observed loop-density effect.
Replacing that yield with a native flag check alone would not preserve the
documented scheduling responsibility.

## Active-call interruption

Every loop shape runs 20 samples for each of context cancellation, module
close, and runtime close at both one and four Go execution threads. All 360
samples passed, including refusal of subsequent work on a closed instance,
survival and state isolation of a sibling after local interruption, and sibling
closure on runtime shutdown.

Across the nine loop-shape/interruption combinations at each thread count:

| Measurement                              |      GOMAXPROCS=1 |    GOMAXPROCS=4 |
| ---------------------------------------- | ----------------: | --------------: |
| Range of median request-to-join latency  |   44.8 to 62.3 µs | 13.2 to 38.8 µs |
| Largest observed request-to-join latency |          0.505 ms |        2.075 ms |
| Range of median trigger scheduling lag   | 19.13 to 19.14 ms |     67 to 87 µs |
| Largest observed trigger scheduling lag  |          40.20 ms |         2.98 ms |

The scheduling column is essential: after guest entry, the host waits a nominal
millisecond before requesting cancellation. With one execution thread, it
typically resumed about 19 ms late. Fast termination _after_ the request does
not establish a one-millisecond timeout guarantee. The warm-up notification
does not prove the exact guest instruction interrupted. These bounded samples
on a shared machine cannot establish a hard realtime guarantee or production SLA.

## Decision

Keep cancellation enabled and keep fresh instances per HTML response. The
reproducer is small enough to share upstream without the parser or a Rust build;
no upstream report has been filed by this change. An upstream investigation can
now evaluate cheaper checkpoints while explicitly preserving single-thread
scheduling, active cancellation, module isolation, and runtime shutdown.

Unrolling this arithmetic loop demonstrates the checkpoint-density effect.
Applying it to a general parser needs separate evaluation. No engine fork, weakened interruption setting,
pooling, or production default change is justified here. A proposed runtime fix
must pass these probes on amd64 and arm64, then improve the real HTML workload
without weakening its existing malformed-input and resource-limit tests.

Large-document/slow-writer coverage, longer memory soaks, artifact reproducibility,
and the separate HTTP integration contract remain open under #114.
