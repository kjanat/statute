.PHONY: build test fuzz bench

build:
	rustup show active-toolchain
	cargo build --manifest-path guest/Cargo.toml --locked --release --lib --target wasm32-unknown-unknown -j 1
	cargo build --manifest-path guest/Cargo.toml --locked --release --bin statute-htmlrewrite-spike -j 1
	install -Dm644 guest/target/wasm32-unknown-unknown/release/statute_htmlrewrite_spike.wasm artifact/rewriter.wasm

test: build
	CGO_ENABLED=0 go test ./...
	CGO_ENABLED=0 go vet ./...
	cargo fmt --manifest-path guest/Cargo.toml --check
	cargo clippy --manifest-path guest/Cargo.toml --locked --all-targets -- -D warnings

fuzz: build
	CGO_ENABLED=0 go test -run '^$$' -fuzz '^FuzzChunkBoundaries$$' -fuzztime=10s -parallel=1 ./...

bench: build
	CGO_ENABLED=0 go test -run '^$$' -bench . -benchmem ./...
