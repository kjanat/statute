# HTML rewriting: execution cost and sustained memory

Follow-up to the [first engine experiment](research-html-rewriting.md) for [#114](https://github.com/kjanat/statute/issues/114). This pass adds measurements and build controls. The default artifact, cancellation policy, fresh-instance ownership, and production Statute behavior are unchanged.

## Where the time goes

On the same Pi 5, the default dense 41,984-byte rewrite measured 16.43 ms with collected output. The instrumented discard-writer benchmark separated the following phases. Values are medians of three one-second samples; medians of components need not sum exactly to the median total.

| Phase                           | Dense 41,984 B | Sparse 42,976 B |
| ------------------------------- | -------------: | --------------: |
| Instance and parser setup       |       1.605 ms |        1.229 ms |
| Feed input in 4-KiB chunks      |      15.202 ms |        4.284 ms |
| Finish and close                |       0.020 ms |        0.013 ms |
| Inside 11 host output callbacks |       0.073 ms |        0.028 ms |

The callback measurement includes range checks, the output copy, and the discard writer. It is nested within the feed/finish measurements. The listener adds measurement overhead; this is not a precise subtraction-based measure of pure parser time.

A separate five-second CPU profile placed 63.5% of samples in `runtime._ExternalCode` and 12.5% directly in wazero's call dispatcher. Go's profile cannot identify the Rust functions inside that external code. Annotated dispatcher samples included cancellation-check return paths. The allocation profile attributed 84.7% of allocated bytes to initial guest memory and memory growth.

### Cancellation control

Three two-second samples measured **16.48 ms with cancellation versus 8.69 ms without it**, using the same fixed dense document, fresh instances, and collected output. This demonstrates a substantial cancellation-related cost in this workload. It does not make the non-cancellable configuration acceptable for serving untrusted HTML.

The normal constructor and memory probe retain `WithCloseOnContextDone(true)`. The disabled control exists only in a named benchmark on finite fixture input. It cannot establish a bounded CPU/wall-clock contract. The remaining cost even in that control still exceeds the earlier native measurement of 1.20 ms.

The [pinned wazero implementation](https://github.com/wazero/wazero/blob/v1.12.0/internal/engine/wazevo/call_engine.go) explains why termination checks return to Go: native guest code must yield so other Go work can run. Any attempt to reduce that cost must preserve interruption and scheduling behavior.

## Build-setting comparisons

[Rust supports explicitly enabling Wasm SIMD](https://doc.rust-lang.org/core/arch/wasm32/). The experiment also increases initial linear memory to 2 MiB, which avoids an early growth allocation for these fixtures. Neither setting changes the host's 32-MiB maximum.

| Setting              | Dense 656-B rewrite | Dense 42-KiB rewrite | Instance setup/close | Dense allocated bytes/op |
| -------------------- | ------------------: | -------------------: | -------------------: | -----------------------: |
| Default              |            1.565 ms |             16.43 ms |             1.081 ms |                3,454,678 |
| `+simd128`           |            1.483 ms |             14.82 ms |             1.133 ms |                3,454,745 |
| Initial memory 2 MiB |            0.907 ms |             15.45 ms |             0.664 ms |                2,594,325 |

These are three-sample medians from sequential runs: initial-memory variant, SIMD, then default. Run order was not randomized. The results support further testing of SIMD and initial-memory sizing; they do not establish a platform-wide gain. Full research tests, including native parity, resource limits, and terminal failures, run against both variants in CI. No default changes are made from this small corpus.

## Sustained-memory observation

Each setting below ran in a fresh process with one compiled engine, four workers, and three 20-second rounds. Every response used a fresh instance, 4-KiB input chunks, and a discard sink. Round expiry canceled and joined unfinished work. Both settings interrupted four requests per round; the table counts completed requests separately.

| Measurement                             |                   Default |      Initial memory 2 MiB |
| --------------------------------------- | ------------------------: | ------------------------: |
| Completed rewrites over 60 seconds      |                    11,537 |                    12,765 |
| Post-GC heap after rounds 1 / 2 / 3     | 1.202 / 1.202 / 1.200 MiB | 1.194 / 1.208 / 1.208 MiB |
| Sampled peak live Go heap               |                  36.1 MiB |                  47.2 MiB |
| Sampled peak process RSS                |                  54.4 MiB |                  56.9 MiB |
| Post-close, post-GC heap                |                 0.319 MiB |                 0.328 MiB |
| RSS after close and explicit scavenging |                  16.6 MiB |                  14.2 MiB |

Goroutine count returned to the pre-load value of two at every round boundary and after shutdown. The observed post-GC heap did not accumulate across these rounds. The initial-memory variant allocated fewer bytes per rewrite but had higher sampled peak heap and RSS in this run. Allocation volume, retained heap, and resident memory answer different questions.

Sampling occurs every 25 ms; it can miss shorter peaks. `/proc/self/status` RSS/high-water values are approximate and process-wide, including JIT code and the Go runtime. Forced GC occurs between rounds; explicit scavenging occurs only before engine creation and after shutdown. These checkpoints affect retention; a production process without them can retain a different amount. The discard writer excludes downstream buffering and slow-client pressure. These one-minute runs do not prove the absence of every leak or supply a production concurrency limit.

## Reproduce and inspect

The [harness instructions](../research/htmlrewrite/README.md#execution-and-memory-probes) describe phase timing, build flags, and the Linux load command. Recorded evidence includes [execution/profile samples](../research/htmlrewrite/results/arm64-execution-2026-10-02.txt) and [memory snapshots](../research/htmlrewrite/results/arm64-memory-2026-10-02.txt), with commands and artifact hashes. All runs used Go 1.27.1, Rust 1.97.1, and `GOMAXPROCS=4` on Linux arm64. CPU profiles also include engine compilation even though benchmark timing excludes it.

## Decision and next experiment

Keep cancellation and fresh-instance isolation. The next focused performance task is a wazero reproducer that separates termination-check overhead from the remaining guest execution cost, followed by measurement of any fix that preserves interruption. Validate promising SIMD/memory settings across larger and malformed documents and amd64 before adopting them.

Pooling would address construction/allocation cost, but it cannot remove the measured guest-feed cost and still needs a reset/discard/isolation contract. Large-document, slow-writer, longer soak, and cross-host artifact-reproducibility work remain open. HTTP middleware and a public rewriting API require the separate integration contract in #114.
