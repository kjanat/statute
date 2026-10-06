NATIVE_BENCH_ITERATIONS ?= 1000
WASM_RUSTFLAGS ?=
LOAD_DURATION ?= 20s
LOAD_ROUNDS ?= 3
LOAD_WORKERS ?= 4
LOAD_COUNT ?= 1024
LOAD_SHAPE ?= dense
HTTP_LOAD_DURATION ?= 5s
HTTP_LOAD_COUNT ?= 32768
HTTP_LOAD_SHAPE ?= dense
HTTP_LOAD_WORKERS ?= 4
HTTP_LOAD_MODE ?= stream
HTTP_LOAD_PACE ?= 0s

.PHONY: build test-disabled test fuzz bench bench-native bench-smoke memory http-load
.PHONY: update-lol-html

build:
	rustup show active-toolchain
	CARGO_TARGET_WASM32_UNKNOWN_UNKNOWN_RUSTFLAGS='$(WASM_RUSTFLAGS)' cargo build --manifest-path guest/Cargo.toml --locked --release --lib --target wasm32-unknown-unknown -j 1
	cargo build --manifest-path guest/Cargo.toml --locked --release --bin statute-htmlrewrite-spike -j 1
	install -Dm644 guest/target/wasm32-unknown-unknown/release/statute_htmlrewrite_spike.wasm artifact/rewriter.wasm

update-lol-html:
	node update-lol-html.mjs '$(VERSION)'

test-disabled:
	CGO_ENABLED=0 go test -tags '' .
	CGO_ENABLED=0 go test -tags htmlrewrite_research .

test: test-disabled build
	node --test feature.test.mjs
	CGO_ENABLED=0 go test -tags statute_htmlrewrite ./...
	CGO_ENABLED=0 go vet -tags statute_htmlrewrite ./...
	CGO_ENABLED=0 go test -tags statute_htmlrewrite,htmlrewrite_research -run '^TestHTTPDocker' .
	CGO_ENABLED=0 go -C ../.. test -tags statute_htmlrewrite,htmlrewrite_research ./internal/researchroute
	CGO_ENABLED=0 go -C ../.. test -tags statute_htmlrewrite,htmlrewrite_research -run '^TestResearchRoute' .
	CGO_ENABLED=0 go vet -tags statute_htmlrewrite,htmlrewrite_research ./...
	cargo fmt --manifest-path guest/Cargo.toml --check
	cargo test --manifest-path guest/Cargo.toml --locked --tests
	node --test update-lol-html.test.mjs
	cargo clippy --manifest-path guest/Cargo.toml --locked --all-targets -- -D warnings

fuzz: build
	CGO_ENABLED=0 go test -tags statute_htmlrewrite -run '^$$' -fuzz '^FuzzChunkBoundaries$$' -fuzztime=10s -parallel=1 .

bench: build
	CGO_ENABLED=0 go test -tags statute_htmlrewrite -run '^$$' -bench . -benchmem ./...

bench-native: build
	cargo run --manifest-path guest/Cargo.toml --locked --release --bin native-bench -- $(NATIVE_BENCH_ITERATIONS)

bench-smoke: build
	CGO_ENABLED=0 go test -tags statute_htmlrewrite -run '^$$' -bench . -benchtime=1x ./...
	$(MAKE) bench-native NATIVE_BENCH_ITERATIONS=1

# Run as one fresh process: compilation and other tests stay outside RSS data.
memory: build
	CGO_ENABLED=0 go test -tags statute_htmlrewrite -c -o artifact/memory.test
	./artifact/memory.test -test.run '^TestMemoryLoad$$' -test.v -test.timeout 10m -load-duration $(LOAD_DURATION) -load-rounds $(LOAD_ROUNDS) -load-workers $(LOAD_WORKERS) -load-count $(LOAD_COUNT) -load-shape $(LOAD_SHAPE)

# Linux server-only RSS/HWM; the origin, native oracle and clients stay in the parent.
http-load: build
	@test "$$(uname -s)" = Linux || { echo "http-load requires Linux /proc memory observations" >&2; exit 1; }
	CGO_ENABLED=0 go test -tags statute_htmlrewrite -c -o artifact/http-load.test
	./artifact/http-load.test -test.run '^TestHTTPLoad$$' -test.v -test.timeout 7m -http-load-duration $(HTTP_LOAD_DURATION) -http-load-count $(HTTP_LOAD_COUNT) -http-load-shape $(HTTP_LOAD_SHAPE) -http-load-workers $(HTTP_LOAD_WORKERS) -http-load-mode $(HTTP_LOAD_MODE) -http-load-pace $(HTTP_LOAD_PACE)
