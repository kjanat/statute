# HTML rewrite feasibility harness

This is the first engine experiment for [issue #114](https://github.com/kjanat/statute/issues/114). It adds no Statute middleware or public API. See the [research report](../../docs/research-html-rewriting.md) for results and remaining work.

## Run

Install Go 1.27.1 and rustup, then run:

```sh
cd research/htmlrewrite
make test
make fuzz
make bench
make bench-native
```

`rust-toolchain.toml` selects Rust 1.97.1, rustfmt, clippy, and the `wasm32-unknown-unknown` target. `make build` builds both the Wasm guest and native output oracle with the committed Cargo lockfile. It copies the guest to `artifact/rewriter.wasm` for Go embedding. The test target uses `CGO_ENABLED=0`; only the parity test launches the native oracle. Rewriting itself starts no subprocesses.

The generated Wasm and Cargo target directory are ignored. This research build needs Rust; ordinary Statute builds do not. Neither root `go test ./...` nor root `make fuzz` includes this separate module. The dedicated research workflow runs it on Linux amd64 and arm64; amd64 also runs the Go race detector. `make bench-smoke` exercises every benchmark with minimal iterations in CI.

`make bench-native NATIVE_BENCH_ITERATIONS=1000` times repeated rewrites inside one Rust process. It excludes process startup and fixture construction. The Go matrix compares direct and batched output with 1-byte and 4-KiB input chunks, dense and sparse selector matches, and small and large documents. Separate benchmarks cover instance creation, concurrent streams, and first-output latency. [Recorded arm64 samples](results/arm64-2026-10-02.txt) accompany the report.

## Architecture contract

- **Owner:** an isolated research harness. No Statute imports, configuration, resolved model, middleware ordering, or runtime dependency changes.
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
| Rust      | 1.97.1, `wasm32-unknown-unknown`, release/LTO/panic-abort               | MIT OR Apache-2.0; bundled components have additional notices |
| wazero    | `github.com/tetratelabs/wazero v1.12.0`; `go.sum`                       | Apache-2.0                                                    |
| Harness   | this repository revision                                                | MIT                                                           |

Source references: [LOL HTML 3.0.1](https://docs.rs/crate/lol_html/3.0.1/source/), [wazero v1.12.0](https://github.com/wazero/wazero/tree/v1.12.0), [Rust licenses](https://github.com/rust-lang/rust/blob/1.97.1/COPYRIGHT).

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
