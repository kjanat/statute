# HTML rewriting: first Wasm experiment

Status: **engine feasibility demonstrated; no production API proposed yet**. This report records the first bounded experiment for [#114](https://github.com/kjanat/statute/issues/114). The issue remains open. A [subsequent execution and memory investigation](research-html-execution-memory.md) measures cancellation overhead, build variants, and sustained fresh-instance load.

## What works

The [HTTP experiment](research-html-http.md) now exercises consumer-selected failure policy, streaming, and existing Statute middleware. The measurements and coverage below describe the earlier engine-only phase.

[The runnable harness](../research/htmlrewrite/README.md) compiles LOL HTML 3.0.1 to `wasm32-unknown-unknown` and executes it with wazero 1.12.0 from cgo-free Go. There is no WASI, JavaScript glue, or subprocess on the rewrite path. The native Rust executable is an output oracle used by tests only.

The harness rewrites an attribute, inserts markup, and removes selected elements. Transformed bytes reach the writer before input EOF. Each stream gets its own parser and guest memory while sharing the compiled module. Sixteen concurrent test streams preserve their independent contents.

Tests compare chunked output with native LOL HTML on Unicode, entities, nested removals, comments, script/textarea content, malformed markup, and empty input. A separate fuzz target compares contiguous and fragmented guest input. Resource tests cover parser memory, linear-memory growth, output limits, short/failed writes, canceled calls, terminal-instance rejection, and engine shutdown.

At this engine-only stage, HTTP response rewriting was untested. The subsequent
[HTTP experiment](research-html-http.md) now tests real Statute responses, including
middleware composition and Docker lifetimes. It remains private: there is no public
rewrite route option or production Wasm dependency in the main Go module.

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

## Initial experiment decision

Continue engine research; **do not integrate this implementation into Statute yet**. Host-neutral Wasm is viable, but the current dense-rewrite cost and allocation volume are too large to justify a public middleware commitment without further measurement and optimization.

The [execution/memory follow-up](research-html-execution-memory.md) measures guest
execution and termination-check overhead, records one-minute memory-load runs, and
compares SIMD/initial-memory settings. The private HTTP adapter has since been
implemented and validated. The public route-scoped API and production integration
remain unimplemented. Fresh instances remain the baseline; mutable-instance reuse
is optional work requiring a reset and discard contract with isolation tests.

## Current #114 coverage and remaining work

| Issue obligation                            | This experiment                                                                                                                                        | Remaining before a production proposal                                                                                                               |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pin upstream, toolchain, artifact, licenses | Exact crate/toolchain/runtime pins, Cargo/Go checksums, explicit build, measured artifact hash, license inventory instructions                         | Cross-host reproducibility, maintained embedded distribution, complete notices and supply-chain policy                                               |
| cgo-free streaming proof                    | Selector/attribute/insertion/removal; output before EOF; no runtime subprocess                                                                         | Configurable policy and Go callback/handle ABI                                                                                                       |
| Parity, fuzzing, failure/lifetime/isolation | Native parity, chunk fuzzing, limits/isolation, active-call interruption, real HTTP disconnects and Docker response lifetimes                          | Callback lifetime/re-entry contract, larger malformed corpus and longer load testing                                                                 |
| Benchmarks                                  | Native/Wasm cases, phase/cancellation profiles, SIMD/memory controls, and one-minute four-worker heap/RSS/retention observations                       | Larger documents, real slow clients, streamed/buffered paths, longer soaks, peak memory and explicit operating budgets                               |
| amd64/arm64 portability                     | Linux compiler execution on both architectures, including amd64 race tests; #142 hosted checks passed                                                  | Production OS/architecture matrix and cross-build/embedded distribution validation                                                                   |
| HTTP integration                            | Private integration covers Cache/Retry/ETag/compression, HEAD/conditions/ranges, protocol interruption, Docker lifetime and final response observation | Public surface/resolved/runtime/export/graph/lint integration, server-owned engine, configurable route programs and rewrite-specific instrumentation |
| Go/no-go and public contract                | Engine feasibility and private HTTP integration demonstrated                                                                                           | Measured operating budget, issue-wide go/no-go and production implementation contract                                                                |

The [HTTP coverage matrix](research-html-http.md#remaining-gate) records the
completed interaction proofs. [#114](https://github.com/kjanat/statute/issues/114)
tracks the remaining delivery work. Earlier measurements above retain their
original toolchain and fixture scope; they are not benchmarks of the latest build.

The production design must address configurable transformations, Go callbacks,
and response-producing route actions, including handlers and static files. No
declarative-only or proxy-only initial-release restriction has been adopted.
Missing prototype coverage is implementation work, not evidence that a route
action must be excluded. Any proposed exclusion needs a concrete architectural
or measured limitation. This does not promise complete Workers API parity.

Failures after emitted output cannot retract that prefix. The HTTP adapter aborts
the response and never appends an unmodified suffix, under either failure policy.
Before transformation begins, consumers explicitly select rejection or untouched
bypass for supported failure cases. This is not an HTML sanitizer.
