# Parser-independent wazero cancellation reproducer

This test-only package isolates compiler-engine termination-check costs without
HTML, Rust, WASI, linear memory, or an allocator. It depends only on wazero
v1.12.0 and the Go standard library. It does not change the HTML engine.

From `research/htmlrewrite`, with Go 1.27.1:

```sh
CGO_ENABLED=0 go test -v -timeout=2m ./cancelprobe
CGO_ENABLED=0 go test -run '^$' -bench . -benchtime=1s -count=3 ./cancelprobe
go test -race -timeout=2m ./cancelprobe
```

The last command requires a host on which Go's race detector can start. CI runs
it on amd64. Both amd64 and arm64 CI run the first two commands, with 500-ms
benchmark samples. No guest artifact or Rust toolchain is required.

For an upstream reproducer, copy this directory into an empty directory, then:

```sh
go mod init example.com/cancelprobe
go get github.com/tetratelabs/wazero@v1.12.0
go test -v -timeout=2m .
go test -run '^$' -bench . -benchtime=1s -count=3 .
```

## Equivalent work, different loop density

`wasm_test.go` encodes three small Wasm modules directly. The following WAT
sketch describes their bodies; `STEP` expands inline without a function call.
Repeat it 1, 16, or 256 times at each marked location:

```wat
;; STEP, with $x an i32 local:
local.get $x local.get $x i32.const 13 i32.shl i32.xor local.set $x
local.get $x local.get $x i32.const 17 i32.shr_u i32.xor local.set $x
local.get $x local.get $x i32.const 5 i32.shl i32.xor local.set $x

;; Import (probe.ready) has signature () -> ().
;; Global $state is mutable i32, initialized to 1, exported as "state".
(func (export "work") (param $groups i32) (param $x i32) (result i32)
  block
    local.get $groups i32.eqz br_if 0
    loop
      ;; STEP repeated batch times
      local.get $groups i32.const 1 i32.sub local.tee $groups br_if 0
    end
  end
  local.get $x)
(func (export "spin") (local $x i32)
  global.get $state local.set $x
  ;; STEP repeated batch times
  local.get $x global.set $state
  call $ready
  loop
    ;; STEP repeated batch times
    local.get $x global.set $state
    br 0
  end)
```

`BenchmarkLoop` always performs 65,536 xorshift32 steps, starting at 42. It
checks the result against Go after every invocation. It excludes compilation
and instantiation; one instance is reused to isolate execution. This is not
an end-to-end HTML benchmark or a proposal to pool request instances.

Each loop shape is measured with `WithCloseOnContextDone(true)` and `false`.
The disabled setting is a finite-work diagnostic control only. The infinite
probe refuses to run without termination checks. Within each batch, enabling
cancellation is the sole configuration difference. Between batches, unrolling
also changes loop bookkeeping, code size, and compiler opportunities; the
comparison is not an exact per-check cost calculation.

## Interruption and isolation

`TestActiveInterruption` covers all three batch sizes, `GOMAXPROCS=1` and `4`,
and context cancellation, module close, and runtime close (20 samples each).
Every sample owns a runtime, a compiled module, and two fresh instances. The
guest changes its own global and signals once, then runs indefinitely. The
host waits a nominal millisecond with the invocation outstanding before
requesting interruption. It joins the call and verifies:

- The interrupted module is closed and rejects subsequent work.
- Context cancellation preserves its error cause.
- Module-local interruption leaves the existing sibling usable with its
  initial global unchanged; runtime shutdown closes the sibling too.
- Failure cleanup also closes the runtime and waits for the invocation.

The warm-up signal proves entry into the guest. It cannot identify the exact
instruction at which cancellation arrives. The test does not inspect concurrently mutated
guest state or promise instruction-level interruption placement.

`stop_*` measures from the host's interruption request until the call is
joined. `trigger_lag_*` separately measures how late the host resumes after
the nominal one-millisecond observation window. In particular, fast stop
latency does not establish a tight deadline when the goroutine requesting
cancellation is itself delayed. These small samples cannot establish a latency
guarantee. The five-second timeouts are failure backstops; no performance
acceptance threshold is enforced.

See the [results and interpretation](../../../docs/research-wazero-cancellation.md).
