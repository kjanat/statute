# HTML rewriting: first Wasm experiment

Status: **engine feasibility demonstrated; no production API proposed yet**. This report records the first bounded experiment for [#114](https://github.com/kjanat/statute/issues/114). The issue remains open.

## What works

[The runnable harness](../research/htmlrewrite/README.md) compiles LOL HTML 3.0.1 to `wasm32-unknown-unknown` and executes it with wazero 1.12.0 from cgo-free Go. There is no WASI, JavaScript glue, or subprocess on the rewrite path. The native Rust executable is an output oracle used by tests only.

The harness rewrites an attribute, inserts markup, and removes selected elements. Transformed bytes reach the writer before input EOF. Each stream gets its own parser and guest memory while sharing the compiled module. Sixteen concurrent test streams preserve their independent contents.

Tests compare chunked output with native LOL HTML on Unicode, entities, nested removals, comments, script/textarea content, malformed markup, and empty input. A separate fuzz target compares contiguous and fragmented guest input. Resource tests cover parser memory, linear-memory growth, output limits, short/failed writes, canceled calls, terminal-instance rejection, and engine shutdown.

HTTP response rewriting remains untested. There is no Statute import, route option, middleware, `resolved` change, or new dependency in the main Go module.

## Initial measurements

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

The current tiny ABI emits many small host callbacks and allocates an output copy for each. Fresh instance creation also contributes overhead. Profiling, output batching, and safe instance reuse need investigation. A native throughput baseline still needs to be measured inside a persistent native process; timing the oracle subprocess would confound it with startup.

## Decision

Continue engine research; **do not integrate this implementation into Statute yet**. Host-neutral Wasm is viable, but the current dense-rewrite cost and allocation volume are too large to justify a public middleware commitment without further measurement and optimization.

The next experiment should measure callback/output batching and warm-instance cost against native LOL HTML on sparse and dense documents. Preserve the present isolation and terminal-error tests before attempting pooling. Only then design the HTTP adapter and public route-scoped API.

## #114 coverage and remaining work

| Issue obligation                            | This experiment                                                                                                                | Remaining before a production proposal                                                                                                            |
| ------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pin upstream, toolchain, artifact, licenses | Exact crate/toolchain/runtime pins, Cargo/Go checksums, explicit build, measured artifact hash, license inventory instructions | Cross-host reproducibility, maintained embedded distribution, complete notices and supply-chain policy                                            |
| cgo-free streaming proof                    | Selector/attribute/insertion/removal; output before EOF; no runtime subprocess                                                 | Configurable policy and Go callback/handle ABI                                                                                                    |
| Parity, fuzzing, failure/lifetime/isolation | Native fixture parity, chunk fuzzing, per-instance bounds and terminal failure tests                                           | Larger malformed corpus, active-execution cancellation latency, callback lifetimes, blocking writers and leak/load testing                        |
| Benchmarks                                  | Initial cold/warm dense rewrite and buffer-copy baseline on arm64                                                              | Native throughput, first-byte latency, sparse/large/slow streams, sustained concurrency, peak memory, profiles and stable samples                 |
| amd64/arm64 portability                     | Local arm64 execution; dedicated CI matrix for both architectures                                                              | Other supported OSes, cross-build/embedded distribution validation; CI results must be read separately from local results                         |
| HTTP integration                            | None; production behavior unchanged                                                                                            | Compression and cache ordering, validators/length/ranges, Retry, HEAD/304, upgrades/SSE/gRPC, backpressure, disconnects, metrics, black-box cases |
| Go/no-go and public contract                | Go for further engine research, no-go for shipping this adapter                                                                | Full issue-wide decision after the measurements and HTTP experiment; separate implementation contract                                             |

Failures after emitted output cannot retract that prefix. The prototype closes the instance and does not append an unmodified suffix. How a future HTTP adapter reports or aborts a committed response remains an explicit design decision. This is not an HTML sanitizer, and users must not treat it as one.
