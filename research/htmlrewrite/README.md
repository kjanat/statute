# HTML rewrite feasibility harness

This is the first engine experiment for [issue #114](https://github.com/kjanat/statute/issues/114). It adds no Statute middleware or public API. See the [research report](../../docs/research-html-rewriting.md) for results and remaining work.

The engine is opt-in with `-tags statute_htmlrewrite`. Without that tag, the
package builds without the generated Wasm file, excludes wazero from its compiled
dependencies and linked binary, and rejects engine construction with an error
that names the required build tag. The Makefile's engine tests and benchmarks
set the tag explicitly. `make test-disabled` needs neither Rust nor the artifact.

The private `htmlrewrite_research` tag only enables the Docker integration seam;
it does not enable the engine. Docker/rewrite tests use both tags. The independent
`cancelprobe` test package continues to exercise wazero directly.

`make test` checks all four feature/hook combinations. Its binary checks verify
that disabled executables contain neither wazero symbols nor the guest bytes,
and that enabled executables contain both. Disabled builds also run with an
overlay removing the guest artifact. An enabled build with that overlay must fail.

This gates the existing research engine. Production rewriting and its public
configuration API remain tracked in #114; normal Statute builds already exclude
this separate research module.

The [production design proposal](../../docs/html-rewriting-design.md) defines
the next implementation sequence and the decisions needed before public integration.

The [Binaryen comparison](../../docs/research-html-binaryen.md) runs six optimizer
variants through correctness, real-parser cancellation, rotated benchmarks and
paced HTTP/memory measurements. `binaryen.mjs` preserves commands and artifact
identities for reproduction on amd64 and arm64.

The [HTTP load report](../../docs/research-html-load.md) documents the larger-page,
slow-client and buffered-ETag measurement contract. `make http-load` runs one
Linux load cell with a separate Statute process; `make memory` accepts
`LOAD_COUNT` and `LOAD_SHAPE` for larger engine-only retention probes.

## Run

Install Go 1.27.1 and rustup, then run:

```sh
cd research/htmlrewrite
make test
make fuzz
make bench
make bench-native
```

`rust-toolchain.toml` selects `nightly-2026-10-04` (Rust 1.101.0-nightly, commit `db8f076d2619ce2585b0380dda06e8da25a40da4`), rustfmt, clippy, and the `wasm32-unknown-unknown` target. `make build` builds both the Wasm guest and native output oracle with the committed Cargo lockfile. It copies the guest to `artifact/rewriter.wasm` for Go embedding. The test target uses `CGO_ENABLED=0`; parity tests and HTTP load setup launch the native oracle for expected output. Rewriting itself starts no subprocesses.

The October 2 benchmark and memory samples used Rust 1.97.1. Those historical results retain their original toolchain attribution; they are not measurements of the current nightly artifact.

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

Production API, HTTP failure semantics, and callback design remain decisions for #114. This experiment does not settle them.

## Research ABI

The guest exports `create(buffered)`, `input_pointer()`, `write(length)`, and `finish()`. Status zero means success; nonzero is terminal to the Go owner. Go writes at most 64 KiB into a fixed guest-owned input region per call. `sink.emit(pointer, length)` is the sole import; the typed host callback checks its memory range and remaining output budget, copies the bytes, and writes synchronously. The host caches function handles within their owning stream; callers must serialize calls and must not re-enter the stream from its writer.

`create(1)` enables a fixed 16-KiB guest output batch, flushed whenever full and after every successful `write` and `finish`. `create(0)` retains direct output for comparison; other values fail. Buffering changes callback granularity and how much prefix can escape before an error, but preserves successful output bytes. On failure, the host closes the instance and discards any pending batch. No buffered suffix is flushed after failure, and batching never waits for the next input call to flush bytes already emitted by the parser.

The fixed Rust policy changes `a.rewrite`'s `href`, appends `<em>inserted</em>` inside it, and removes `.remove` subtrees. There are no Go element handles, configurable selectors, or arbitrary Go callbacks. Selecting those is deliberately outside this small ABI experiment.

LOL HTML's parser budget is 1 MiB and wazero's per-instance linear-memory ceiling is 32 MiB. The writer has a separate byte budget. These are not a total process-memory bound: compiled code, Go allocations, output storage, and simultaneous instances add to it. There is no concurrency admission limit yet. The host explicitly selects wazero's compiler engine, with its default WebAssembly Core 2 features. Unsupported hosts fail; there is no interpreter fallback.

Context cancellation is enabled for guest execution. Tests cover cancellation before a call and cancellation while a controlled writer blocks, followed by release and terminal cleanup. They do not establish a worst-case interruption latency. An arbitrary Go `io.Writer` can block inside the host callback, so a future HTTP adapter must separately handle write deadlines and downstream disconnects. Never describe this as a general CPU-time or wall-clock budget.

## Dependencies and artifact provenance

| Component | Pin                                                                     | License                                                       |
| --------- | ----------------------------------------------------------------------- | ------------------------------------------------------------- |
| LOL HTML  | crates.io `lol_html = "=3.0.1"`; archive checksum in `guest/Cargo.lock` | BSD-3-Clause                                                  |
| Rust      | `nightly-2026-10-03`, `wasm32-unknown-unknown`, release/LTO/panic-abort | MIT OR Apache-2.0; bundled components have additional notices |
| wazero    | `github.com/tetratelabs/wazero v1.12.0`; `go.sum`                       | Apache-2.0                                                    |
| Harness   | this repository revision                                                | MIT                                                           |

Source references: [LOL HTML 3.0.1](https://docs.rs/crate/lol_html/3.0.1/source/), [wazero v1.12.0](https://github.com/wazero/wazero/tree/v1.12.0), [Rust licenses](https://github.com/rust-lang/rust/blob/0abfedbc7cd4e725f126913880c95800394f7c37/COPYRIGHT).

Inspect the complete locked Rust license inventory with:

```sh
cargo metadata --manifest-path guest/Cargo.toml --locked --format-version 1 |
  jq -r '.packages[] | [.name, .version, .license] | @tsv'
sha256sum artifact/rewriter.wasm
```

The graph includes MPL-2.0 crates (`cssparser`, `cssparser-macros`, `dtoa-short`, `selectors`), Zlib (`foldhash`), and Unicode-3.0 obligations (`unicode-ident`), in addition to MIT/Apache/BSD alternatives. An embedded production distribution needs a complete target-specific notice/source bundle, including the Rust standard library; the top-level LOL HTML license alone is not sufficient. No prebuilt binary is distributed here.

The batched Linux arm64 build is 563,685 bytes with SHA-256:

```text
821a59adaacf94d28afe76e07b6a3012b05d64bd47cc1aacf35c8b81fec9dd18
```

A fresh `--target-dir guest/target/rebuild` build on the same host produced identical bytes. Cross-host reproducibility has not been established. The workflow builds from the locked source and prints its own checksum. Artifact signing, reproducible release packaging, and automated notice generation remain open research work.
