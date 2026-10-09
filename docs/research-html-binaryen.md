# Binaryen optimization comparison

This experiment compares six build variants for [#114](https://github.com/kjanat/statute/issues/114).
The input is one Rust-built LOL HTML guest. The public API, production runtime,
cancellation policy and fresh-instance ownership are unchanged.

## Contract and method

The research harness owns artifact generation, test processes and measurements.
Each variant retains the existing memory, parser and output limits, host imports,
native-output oracle, error propagation and instance isolation. Failure of
artifact validation, parity, cancellation, HTTP delivery or process cleanup fails
the comparison. The parent joins every parser worker before retiring its engine.

All optimized variants receive `--enable-bulk-memory-opt` and
`--enable-nontrapping-float-to-int`: the stripped Rust input already contains
these instructions, and Binaryen rejects it without these compatibility flags.
The optimization sequences are:

| Variant           | Passes                         |
| ----------------- | ------------------------------ |
| baseline          | Unmodified Rust output         |
| o3                | `-O3`                          |
| oz                | `-Oz`                          |
| oz-twice          | `-Oz -Oz`                      |
| rereloop-o3       | `--flatten --rereloop -O3`     |
| rereloop-oz-twice | `--flatten --rereloop -Oz -Oz` |

The runner preserves repeated optimization passes and tests their ordering.
Go build overlays embed each artifact in a separate test executable; the ordinary
embedded artifact remains the baseline. Tests verify the embedded SHA-256 against
the manifest before performance comparisons. Binaryen 133 release archives are
checksum-pinned in the comparison workflow.

Each executable runs the complete research test package with the private Docker
integration tag, including native parity, malformed/chunked inputs, resource
limits, isolation, Cache/Retry/ETag/compression, HTTP/2 and HTTP/3 interruption,
and Docker response lifetimes. Parser-independent cancellation tests remain in
the existing research workflow.

Benchmarks use three independent processes per variant, rotating variant order
between rounds. Each case runs for at least 500 ms. Dense/sparse inputs contain
16, 1,024 or 32,768 fragments, with 4-KiB feeds, bounded output and a discard sink.
Cold engine creation, instance creation/closure and first-output timing are
separate benchmarks. Cancellation stays enabled in every measured configuration.

The real-parser interruption test continuously feeds 61,500-byte dense chunks,
waits for its first output, then schedules cancellation one millisecond later.
It records scheduling lateness separately from cancellation-to-join latency,
with 20 samples at both GOMAXPROCS=1 and 4. Cancellation can occur within or
between guest calls; this measures the complete parser workload. Every sample
must close its instance and reject further writes; a subsequent fresh instance
must still work. These timings have no hard real-time guarantee.

Load comparisons use four paced clients, a three-second admission window and
the 5.13-MiB dense input for both streamed and ETag-buffered delivery. Responses
are checked against native output, including a post-load recovery request.
The [HTTP measurement boundaries](research-html-load.md#measurement-contract)
apply. Three three-second engine-only rounds separately report post-GC retention
on the 1.28-MiB dense input. These short comparisons supplement the longer baseline
observations; they do not replace them.

## Reproduce

Install mise, then run:

```sh
cd research/htmlrewrite
mise install
mise exec -- node binaryen.mjs build
mise exec -- node binaryen.mjs test
mise exec -- node binaryen.mjs bench
mise exec -- node binaryen.mjs load
```

Commands, tool versions, full outputs, artifact hashes and exact flags are written
under `artifact/binaryen/`. The dedicated GitHub Actions comparison repeats all
six variants on Linux amd64 and arm64 and uploads those files, including failure
logs. Run measurements serially without competing builds.

## Raspberry Pi 5 results, 2026-10-05

Linux arm64, Cortex-A76, four execution threads, Go 1.27.1, CGO_ENABLED=0,
Rust nightly-2026-10-04 (`db8f076d2619ce2585b0380dda06e8da25a40da4`), LLVM 23.1.1,
Binaryen 133. This is the shared development Pi, with background services and
about 8.9 GiB swap occupied before measurement. The baseline artifact SHA-256 is
`9611994c63f3f447c11750f0e368f44547486860b5b7743666064aefe1976781`.
The earlier HTTP load report used an older Rust nightly; comparisons here use
this experiment's own baseline throughout.

Median of three independent benchmark processes; times are milliseconds.
Dense/sparse columns are the approximately 1.3-MiB inputs; first-output uses the
41-KiB dense fixture. Cold allocation is cumulative Go allocation per engine
creation/closure. Retained memory and peak RSS are measured separately below.

| Variant           | Wasm bytes | Cold engine | Cold allocation MiB | Instance | First output | Dense rewrite | Sparse rewrite |
| ----------------- | ---------: | ----------: | ------------------: | -------: | -----------: | ------------: | -------------: |
| baseline          |     574007 |      331.12 |               19.96 |    1.434 |        2.693 |        460.31 |         128.24 |
| o3                |     498624 |      404.50 |               40.69 |    1.292 |        2.668 |        446.77 |         125.41 |
| oz                |     485011 |      387.42 |               39.91 |    1.471 |        2.661 |        446.54 |         119.02 |
| oz-twice          |     484633 |      390.03 |               39.89 |    1.370 |        2.745 |        450.67 |         117.57 |
| rereloop-o3       |     497765 |      430.74 |               40.01 |    1.399 |        2.716 |        449.42 |         124.38 |
| rereloop-oz-twice |     482719 |      385.43 |               39.38 |    1.507 |        2.648 |        444.51 |         120.31 |

All six passed the complete research suite, including private Docker tests and
240 real-parser interruption samples. Single-thread median cancellation-trigger
lateness stayed near 19.1 ms for every variant. Median request-to-join latency
was 66 to 78 microseconds with one execution thread and 36 to 58 microseconds with four;
the largest observed request-to-join delay across these samples was 0.272 ms.
The optimization did not remove the single-thread scheduling delay.

Binaryen's static loop counts are 861 (baseline), 851 (`-O3`), 817 (`-Oz`
and twice-`-Oz`), 853 (rereloop/`-O3`) and 818 (`rereloop-oz-twice`). Static
instruction counts alone cannot explain the runtime cost of cancellation checkpoints; dynamic
execution frequencies and individual checkpoint costs were not instrumented.

The size benefit is clear: 13.1 to 15.9% smaller artifacts. The second `-Oz` saves
378 bytes relative to one round. In these samples, large dense rewrites took
2.1 to 3.4% less time and sparse rewrites 2.2 to 8.3% less time. Cold engine creation
took 16.4 to 30.1% longer and allocated roughly twice as much Go memory. Smaller
Wasm therefore did not translate to cheaper wazero compilation. Small-document
and instance results vary by sequence; there is no uniform winner across the
measured operations. Three samples describe this comparison without establishing
statistical significance for the small throughput differences.

### Paced HTTP and retained memory

Each HTTP cell contains four completed, byte-verified responses. Completion is
the largest observed duration (also p95 with four samples). RSS high-water marks
cover the server process lifetime, including compilation, warm-up and the
admission window. Memory columns use MiB; completion uses seconds.

| Variant           | Stream completion | ETag completion | Stream RSS HWM | ETag RSS HWM | Engine heap after round 3 | Heap after close |
| ----------------- | ----------------: | --------------: | -------------: | -----------: | ------------------------: | ---------------: |
| baseline          |            11.284 |          13.774 |          50.48 |       184.80 |                     2.745 |            0.621 |
| o3                |            11.280 |          13.596 |          60.30 |       180.55 |                     2.629 |            0.621 |
| oz                |            11.289 |          13.506 |          67.30 |       188.84 |                     2.614 |            0.619 |
| oz-twice          |            11.282 |          13.600 |          76.97 |       194.72 |                     2.620 |            0.624 |
| rereloop-o3       |            11.295 |          13.590 |          60.36 |       185.45 |                     2.626 |            0.619 |
| rereloop-oz-twice |            11.293 |          13.554 |          67.97 |       191.02 |                     2.607 |            0.614 |

Client pacing dominates streamed completion. Optimization does not remove the
ETag buffering cost: those processes peak around 181 to 195 MiB versus 50 to 77 MiB
for streaming. Every recovery request passed. All six engine-only processes
returned to two goroutines after every round and after engine closure.

### Decision

Do not adopt the `--flatten --rereloop -Oz -Oz` sequence as a default on these
results. It makes the smallest artifact, but saves only 2,292 bytes beyond plain
`-Oz`, does not resolve the
single-thread cancellation scheduling delay, and incurs the same cold-compile
trade-off. Plain `-Oz` is the simpler size-oriented candidate; retaining the Rust
output is reasonable when cold initialization matters more. No production or
research default is changed by this comparison.

These measurements apply to Statute's embedded LOL HTML/wazero engine.
The hosted amd64/arm64 comparison retains the same six cases so
the Pi's runtime trade-offs can be checked on both architectures.

Raw local evidence: [build and static metrics](../research/htmlrewrite/results/binaryen-arm64-build.txt),
[benchmarks](../research/htmlrewrite/results/binaryen-arm64-bench.txt),
[HTTP and memory loads](../research/htmlrewrite/results/binaryen-arm64-load.txt),
and [test summary](../research/htmlrewrite/results/binaryen-arm64-tests.txt).
