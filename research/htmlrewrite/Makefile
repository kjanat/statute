NATIVE_BENCH_ITERATIONS ?= 1000
WASM_RUSTFLAGS ?=
LOAD_DURATION ?= 20s
LOAD_ROUNDS ?= 3
LOAD_WORKERS ?= 4

.PHONY: build test fuzz bench bench-native bench-smoke memory

build:
	rustup show active-toolchain
	CARGO_TARGET_WASM32_UNKNOWN_UNKNOWN_RUSTFLAGS='$(WASM_RUSTFLAGS)' cargo build --manifest-path guest/Cargo.toml --locked --release --lib --target wasm32-unknown-unknown -j 1
	cargo build --manifest-path guest/Cargo.toml --locked --release --bin statute-htmlrewrite-spike -j 1
	install -Dm644 guest/target/wasm32-unknown-unknown/release/statute_htmlrewrite_spike.wasm artifact/rewriter.wasm

test: build
	CGO_ENABLED=0 go test ./...
	CGO_ENABLED=0 go vet ./...
	cargo fmt --manifest-path guest/Cargo.toml --check
	cargo clippy --manifest-path guest/Cargo.toml --locked --all-targets -- -D warnings

fuzz: build
	CGO_ENABLED=0 go test -run '^$$' -fuzz '^FuzzChunkBoundaries$$' -fuzztime=10s -parallel=1 .

bench: build
	CGO_ENABLED=0 go test -run '^$$' -bench . -benchmem ./...

bench-native: build
	cargo run --manifest-path guest/Cargo.toml --locked --release --bin native-bench -- $(NATIVE_BENCH_ITERATIONS)

bench-smoke: build
	CGO_ENABLED=0 go test -run '^$$' -bench . -benchtime=1x ./...
	$(MAKE) bench-native NATIVE_BENCH_ITERATIONS=1

# Run as one fresh process: compilation and other tests stay outside RSS data.
memory: build
	CGO_ENABLED=0 go test -c -o artifact/memory.test
	./artifact/memory.test -test.run '^TestMemoryLoad$$' -test.v -test.timeout 10m -load-duration $(LOAD_DURATION) -load-rounds $(LOAD_ROUNDS) -load-workers $(LOAD_WORKERS)
