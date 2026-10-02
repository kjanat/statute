# HTML rewriting: first Wasm experiment

Status: **engine feasibility demonstrated; no production API proposed yet**. This report records the first bounded experiment for [#114](https://github.com/kjanat/statute/issues/114). The issue remains open.

## What works

[The runnable harness](../research/htmlrewrite/README.md) compiles LOL HTML 3.0.1 to `wasm32-unknown-unknown` and executes it with wazero 1.12.0 from cgo-free Go. There is no WASI, JavaScript glue, or subprocess on the rewrite path. The native Rust executable is an output oracle used by tests only.

The harness rewrites an attribute, inserts markup, and removes selected elements. Transformed bytes reach the writer before input EOF. Each stream gets its own parser and guest memory while sharing the compiled module. Sixteen concurrent test streams preserve their independent contents.

Tests compare chunked output with native LOL HTML on Unicode, entities, nested removals, comments, script/textarea content, malformed markup, and empty input. A separate fuzz target compares contiguous and fragmented guest input. Resource tests cover parser memory, linear-memory growth, output limits, short/failed writes, canceled calls, terminal-instance rejection, and engine shutdown.

HTTP response rewriting remains untested. There is no Statute import, route option, middleware, `resolved` change, or new dependency in the main Go module.

## Initial measurements before optimization

Measured on a Raspberry Pi 5, Linux arm64, Go 1.27.1, Rust 1.97.1. One short local run used:

```sh
CGO_ENABLED=0 go test -run '^$' -bench . -benchtime=200ms -count=1 ./...
```

| Operation                                         |    Input |  Time/op | Go bytes allocated/op | Go allocations/op |
| ------------------------------------------------- | -------: | -------: | --------------------: | ----------------: |
| Warm compiled engine, new instance, dense rewrite |    656 B | 2.314 ms |             3,075,952 |             2,389 |
| Warm compiled engine, new instance, dense rewrite | 41,984 B | 57.29 ms |             5,405,184 |            81,099 |
| Buffer copy without parsing/rewriting             |    656 B |   378 ns |                   704 |                 1 |
| Buffer copy without parsing/rewriting             | 41,984 B | 16.78 µs |                49,152 |                 1 |
| Cold engine construction, compile, close          |      N/A | 321.9 ms |            21,018,360 |            42,326 |

The fixture repeats a matched anchor 16 or 1,024 times. Rewrite measurements include instance creation, guest calls, output copies, result buffering, and teardown. The copy baseline performs no HTML work and is not an equivalent implementation. Go allocation counters are not peak resident memory. The cold result had one iteration. Longer repeated runs are needed for stable performance or production capacity estimates.

These measurements describe [the initial implementation](https://github.com/kjanat/statute/tree/5df52259a0f8a90945bc24dc49f16667244ab158/research/htmlrewrite). The follow-up below profiles and changes its callback path without introducing instance pooling.

## Profile-guided changes

A subsequent three-sample, two-second-per-sample profile of the original dense 41,984-byte case measured a median 50.95 ms and about 81,097 Go allocations per rewrite. The reflection-based callback path accounted for roughly 62% of sampled CPU time cumulatively. Replacing `WithFunc` with wazero's typed `WithGoModuleFunction` reduced the median to 17.31 ms and about 14,525 allocations before changing guest buffering.

The guest now batches output in a fixed 16-KiB buffer. It flushes on a full buffer, after each successful input call, and at EOF. With 4-KiB input chunks, the dense fixture makes 11 host callbacks compared with 13,314 in direct mode. Both modes remain available in the benchmark and parity tests. Full-batch failures discard the remaining buffered suffix and close the instance.

Tiny input chunks exposed a second allocation source: resolving exported functions on every call constructs fresh wazero call state. Function handles now belong to the stream and are reused only by that stream's serialized calls. For the dense one-byte-chunk fixture, allocated Go bytes fell from roughly 506 MB per response in the intermediate implementation to 11.5 MB. This still makes extremely small writes expensive. Resident memory requires separate measurement.

## Follow-up measurements

The [raw samples](../research/htmlrewrite/results/arm64-2026-10-02.txt) record sequential native and Go runs on the same Pi with the same toolchain. Native timings have three samples of 1,000 rewrites inside one process, excluding process startup. Go timings have three 500-ms samples. Both construct a fresh parser per rewrite and collect the output. Go additionally instantiates and tears down a Wasm module. The shared compiled module is warm.

Dense input has a matching anchor in every fragment. Sparse input has one matching anchor every 32 fragments, including the first; the other fragments are unmatched paragraphs. Each fixture has either 16 or 1,024 fragments. Values below are medians; the timing samples are short and do not establish production capacity.

| Fixture / input chunk   | Native time | Batched Wasm time | Wasm Go allocations/op | Host callbacks/op |
| ----------------------- | ----------: | ----------------: | ---------------------: | ----------------: |
| Dense 656 B / 4 KiB     |    21.76 µs |          1.573 ms |                  1,125 |                 1 |
| Dense 41,984 B / 4 KiB  |    1.199 ms |          16.19 ms |                  1,189 |                11 |
| Sparse 671 B / 4 KiB    |    10.05 µs |          1.501 ms |                  1,124 |                 1 |
| Sparse 42,976 B / 4 KiB |    407.9 µs |          5.357 ms |                  1,188 |                11 |
| Dense 41,984 B / 1 B    |    3.423 ms |          76.50 ms |                220,268 |             9,216 |
| Sparse 42,976 B / 1 B   |    2.428 ms |          71.78 ms |                253,003 |            36,992 |

Fresh instance construction and closure alone take about 1.328 ms and allocate 3.04 MB in Go. The dense first-output benchmark emits its first bytes after about 2.733 ms, including instance setup and the first 4-KiB input call. A four-worker parallel benchmark reports 5.896 ms/op as aggregate throughput cost; that number is not individual request latency. Its three samples have 114 to 122 total operations, so it is not a sustained-load result.

The final dense 4-KiB-chunk result is approximately 3.1 times faster than the remeasured original, with about 98.5% fewer Go allocations. It remains approximately 13.5 times slower than native for that case. Batching reduced allocation count substantially; its time improvement over the typed direct callback is modest. The raw direct/batched samples retain that comparison.

Reproduce the runs from `research/htmlrewrite`:

```sh
make bench-native NATIVE_BENCH_ITERATIONS=1000
CGO_ENABLED=0 go test -run '^$' -bench 'Benchmark(Matrix|Instance|Parallel|FirstOutput)$' -benchtime=500ms -count=3
CGO_ENABLED=0 go test -run '^$' -bench 'BenchmarkRewrite/bytes=41984$' -benchtime=2s -cpuprofile artifact/rewrite.cpu -memprofile artifact/rewrite.mem -o artifact/rewrite.test
go tool pprof -top artifact/rewrite.test artifact/rewrite.cpu
go tool pprof -top -alloc_space artifact/rewrite.test artifact/rewrite.mem
```

## Decision

Continue engine research; **do not integrate this implementation into Statute yet**. Host-neutral Wasm is viable, but the current dense-rewrite cost and allocation volume are too large to justify a public middleware commitment without further measurement and optimization.

The next performance investigation should isolate remaining guest-execution cost and measure retained/peak memory under sustained load. Instance creation costs about 1.3 ms; the dense rewrite costs about 16 ms. Any reuse experiment needs a reset and discard contract with isolation tests before it can replace fresh instances. The HTTP adapter and public route-scoped API still require their own design.

## #114 coverage and remaining work

| Issue obligation                            | This experiment                                                                                                                               | Remaining before a production proposal                                                                                                               |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pin upstream, toolchain, artifact, licenses | Exact crate/toolchain/runtime pins, Cargo/Go checksums, explicit build, measured artifact hash, license inventory instructions                | Cross-host reproducibility, maintained embedded distribution, complete notices and supply-chain policy                                               |
| cgo-free streaming proof                    | Selector/attribute/insertion/removal; output before EOF; no runtime subprocess                                                                | Configurable policy and Go callback/handle ABI                                                                                                       |
| Parity, fuzzing, failure/lifetime/isolation | Direct/batched/native fixture parity, chunk fuzzing, batch bounds and terminal failures, controlled writer backpressure and cancellation      | Larger malformed corpus, active-execution cancellation latency, future Go element-callback lifetimes, real downstream disconnects, leak/load testing |
| Benchmarks                                  | Native/Wasm dense and sparse cases, 1-byte/4-KiB chunks, first output, per-instance cost, short concurrent runs, callback/allocation profiles | Larger documents, real slow clients, sustained concurrency, peak/retained memory, safe warm-instance reuse and stable platform comparisons           |
| amd64/arm64 portability                     | Initial PR CI passed on both architectures, including amd64 race tests; local follow-up measurements on arm64                                 | Final-head CI, other supported OSes and cross-build/embedded distribution validation                                                                 |
| HTTP integration                            | None; production behavior unchanged                                                                                                           | Compression and cache ordering, validators/length/ranges, Retry, HEAD/304, upgrades/SSE/gRPC, backpressure, disconnects, metrics, black-box cases    |
| Go/no-go and public contract                | Go for further engine research, no-go for shipping this adapter                                                                               | Full issue-wide decision after the measurements and HTTP experiment; separate implementation contract                                                |

Failures after emitted output cannot retract that prefix. The prototype closes the instance and does not append an unmodified suffix. How a future HTTP adapter reports or aborts a committed response remains an explicit design decision. This is not an HTML sanitizer, and users must not treat it as one.
