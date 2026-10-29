# HTML rewriting research

`research/htmlrewrite` runs LOL HTML in WebAssembly through wazero. It tests
streamed HTML changes and Go callbacks for [#114](https://github.com/kjanat/statute/issues/114).

Enable execution with `-tags statute_htmlrewrite`. Builds without this tag return
an unavailable error and link neither wazero nor the guest artifact.
`make test-disabled` checks this without Rust or a generated Wasm file.

`htmlrewrite_research` enables Docker integration tests; use both tags for those
tests. `make test` exercises all four tag combinations and checks linked binaries,
including builds with the guest file removed.

[Programs and callbacks](../../docs/research-html-programs.md) describes configuring
HTML transformations and testing Go callbacks.

[Implementation plan](../../docs/html-rewriting-design.md) covers the HTTP adapter,
route configuration and release qualification.

[Binaryen measurements](../../docs/research-html-binaryen.md) compare six optimizer
variants. Run `binaryen.mjs` to repeat them on amd64 or arm64.

[HTTP load measurements](../../docs/research-html-load.md) cover larger pages,
slow clients and buffered ETags. `make http-load` runs Statute as a separate
process. `make memory` measures the engine with `LOAD_COUNT` and `LOAD_SHAPE`.

## Run

Install mise, then run:

```sh
mise install --monorepo
mise run //research/htmlrewrite:test
mise run //research/htmlrewrite:test-race
mise -C research/htmlrewrite exec -- make fuzz
mise -C research/htmlrewrite exec -- make bench
mise -C research/htmlrewrite exec -- make bench-native
```

`make build` writes the Go-embedded guest to `artifact/rewriter.wasm` and builds the native executable used for output comparisons. Tests use `CGO_ENABLED=0`.

The October 2 benchmark and memory samples used Rust 1.97.1.

The generated Wasm and Cargo target directory are ignored. This research build needs Rust; ordinary Statute builds do not. Neither root `go test ./...` nor root `make fuzz` includes this separate module. The dedicated research workflow runs it on Linux amd64 and arm64; amd64 also runs the Go race detector. `make bench-smoke` exercises every benchmark with minimal iterations in CI.

`make bench-native NATIVE_BENCH_ITERATIONS=1000` times repeated rewrites inside one Rust process. It excludes process startup and fixture construction. The Go matrix compares direct and batched output with 1-byte and 4-KiB input chunks, dense and sparse selector matches, and small and large documents. Separate benchmarks cover instance creation, concurrent streams, and first-output latency. [Recorded arm64 samples](results/arm64-2026-10-02.txt) accompany the report.

## Execution and memory probes

`BenchmarkPhases` times instance/parser setup, input feeding, and finish/close separately. A test-only wazero listener measures time inside the host `emit` function, including range checks and the copy into a discard writer. That time is **nested inside** the write/finish timings; do not add it again. These instrumented measurements include observer overhead and exclude compilation. They do not attribute individual Rust functions or measure only parser instructions.

`BenchmarkCancellationCost` compares the normal engine against a deliberately non-cancellable control on a fixed, finite fixture. The control is unsafe for arbitrary input and is not a supported engine policy. The normal engine and memory probe retain cancellation, memory limits, and fresh instances.

On Linux, run the load probe as its own process:

```sh
make memory LOAD_DURATION=20s LOAD_ROUNDS=3 LOAD_WORKERS=4
```

It shares one compiled engine, feeds dense 42-KiB documents in 4-KiB chunks, and discards output without collecting whole responses. Each worker owns and closes its stream; round expiry cancels and joins outstanding work. Completed and interrupted requests are reported separately. Samples every 25 ms record Go heap and `/proc/self/status` RSS; round boundaries record post-GC retention. Shutdown records both post-GC and explicitly scavenged memory. Forced GC/scavenging is outside the timed load. RSS and the kernel high-water mark are approximate process-wide readings; samples can miss transient peaks. This does not measure real-client buffering or establish an admission limit or absence of all leaks. The ordinary Linux test suite runs a 100-ms smoke case without memory/performance thresholds.

Compare build settings with the same tests and benchmarks:

```sh
make test WASM_RUSTFLAGS='-Ctarget-feature=+simd128'
CGO_ENABLED=0 go test -tags statute_htmlrewrite -run '^$' -bench '^Benchmark(Phases|Rewrite|Instance)$' -benchtime=1s -count=3
make test WASM_RUSTFLAGS='-Clink-arg=--initial-memory=2097152'
CGO_ENABLED=0 go test -tags statute_htmlrewrite -run '^$' -bench '^Benchmark(Phases|Rewrite|Instance)$' -benchtime=1s -count=3
make build WASM_RUSTFLAGS=
```

`WASM_RUSTFLAGS` applies only to the Wasm build; it does not change the native oracle. Direct Go commands embed whichever artifact was built last, so restore the empty setting for baseline comparisons. Neither setting is selected by default. CI tests both variants on amd64 and arm64, including parity, limits, and terminal failures. The 32-MiB guest ceiling remains unchanged. A larger initial memory affects every instance, including small responses.

## Architecture contract

The [HTTP integration experiment](../../docs/research-html-http.md) adds a private
route-owned transport adapter and tests consumer-selected fail-open/fail-closed
behavior. Separate-process integration tests use the local Statute module to
exercise its real Cache, Retry, and Compress middleware. The engine-only
boundaries below remain unchanged; the HTTP adapter is not a production API.

- **Owner:** an isolated research harness. Only the HTTP integration tests import Statute; production configuration, resolved model, middleware ordering, and runtime dependencies are unchanged.
- **Invariants:** one parser per response; byte ordering preserved; no raw-input fallback following a rewrite error; no filesystem, network, WASI, or JavaScript guest imports.
- **Boundaries:** Go context/writer → guest input memory → LOL HTML → synchronous host output callback. The native executable uses the same crate and rewrite policy as the guest.
- **Failure:** invalid lifecycle calls, parser errors, memory/output limits, canceled calls, and writer errors terminate that instance. Previously emitted bytes cannot be rolled back. Closing the shared engine closes its instances.
- **State:** the engine owns the runtime and shared compiled module; each stream owns one instantiated module, parser, function handles, context, output batch, and output budget. The caller closes abandoned streams and closes the engine after use. Instances are never pooled or reused after failure.
- **Cross-feature interactions:** concurrent streams share compiled code and retain independent mutable parser state. HTTP compression, cache, Retry, routing, health, Docker, and Statute startup/shutdown are untouched and untested by this experiment.
- **Acceptance:** transformation before EOF; direct/batched/native parity; malformed/UTF-8 input; independent concurrent instances; failure isolation; output/parser/linear-memory bounds; batch boundaries; EOF flushing; synchronous backpressure; cancellation during a host callback; shutdown; bounded chunk fuzzing.

The [design](../../docs/html-rewriting-design.md) covers the next steps: the HTTP adapter, public middleware and release qualification.

## Guest calls

The guest exports `create(buffered)`, `input_pointer()`, `write(length)`, and `finish()`. Status zero means success; nonzero is terminal to the Go owner. Go writes at most 64 KiB into a fixed guest-owned input region per call. `sink.emit(pointer, length)` emits output; the typed host callback checks its memory range and remaining output budget, copies the bytes, and writes synchronously. The host caches function handles within their owning stream; callers must serialize calls and must not re-enter the stream from its writer.

`create(1)` enables a fixed 16-KiB guest output batch, flushed whenever full and after every successful `write` and `finish`. `create(0)` retains direct output for comparison; other values fail. Buffering changes callback granularity and how much prefix can escape before an error, but preserves successful output bytes. On failure, the host closes the instance and discards any pending batch. No buffered suffix is flushed after failure, and batching never waits for the next input call to flush bytes already emitted by the parser.

LOL HTML's parser budget is 1 MiB and wazero's per-instance linear-memory ceiling is 32 MiB. The writer has a separate byte budget. These are not a total process-memory bound: compiled code, Go allocations, output storage, and simultaneous instances add to it. There is no concurrency admission limit yet. The host explicitly selects wazero's compiler engine, with its default WebAssembly Core 2 features. Unsupported hosts fail; there is no interpreter fallback.

Context cancellation is enabled for guest execution. Tests cover cancellation before a call and cancellation while a controlled writer blocks, followed by release and terminal cleanup. They do not establish a worst-case interruption latency. An arbitrary Go `io.Writer` can block inside the host callback, so a future HTTP adapter must separately handle write deadlines and downstream disconnects. Never describe this as a general CPU-time or wall-clock budget.

## Dependencies and artifact provenance

| Component | Pin                                                                                                                                 | License                                                       |
| --------- | ----------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| LOL HTML  | [Cargo.toml](guest/Cargo.toml) and [Cargo.lock](guest/Cargo.lock), with the [local matcher ownership patch](guest/vendor/README.md) | BSD-3-Clause                                                  |
| Rust      | [.mise.toml](.mise.toml); release/LTO/panic-abort settings in [Cargo.toml](guest/Cargo.toml)                                        | MIT OR Apache-2.0; bundled components have additional notices |
| wazero    | [go.mod](go.mod) and [go.sum](go.sum)                                                                                               | Apache-2.0                                                    |
| Harness   | this repository revision                                                                                                            | MIT                                                           |

[Vendor provenance](guest/vendor/README.md) includes the LOL HTML archive checksum and local patch.

Inspect the complete locked Rust license inventory with:

```sh
cargo metadata --manifest-path guest/Cargo.toml --locked --format-version 1 |
  jq -r '.packages[] | [.name, .version, .license] | @tsv'
sha256sum artifact/rewriter.wasm
```

The graph includes MPL-2.0 crates (`cssparser`, `cssparser-macros`, `dtoa-short`, `selectors`), Zlib (`foldhash`), and Unicode-3.0 obligations (`unicode-ident`), in addition to MIT/Apache/BSD alternatives. An embedded production distribution needs a complete target-specific notice/source bundle, including the Rust standard library; the top-level LOL HTML license alone is not sufficient. No prebuilt binary is distributed here.

The recorded LOL HTML 3.0.1 batched Linux arm64 build was 563,685 bytes with SHA-256:

```text
821a59adaacf94d28afe76e07b6a3012b05d64bd47cc1aacf35c8b81fec9dd18
```

A fresh `--target-dir guest/target/rebuild` build on the same host produced identical bytes. These measurements describe the recorded 3.0.1 artifact. Cross-host reproducibility has not been established. The workflow builds from the locked source and prints its own checksum. Artifact signing, reproducible release packaging, and automated notice generation remain open research work.
