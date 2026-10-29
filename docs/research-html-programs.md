# HTML rewrite programs and callbacks

HTML response rewriting is being developed in `research/htmlrewrite` for
[#114](https://github.com/kjanat/statute/issues/114). This Go package runs LOL HTML
in WebAssembly and lets Go callbacks inspect and modify parsed HTML. Public route
integration follows the [implementation plan](html-rewriting-design.md).

## Programs

A program lists CSS selectors and changes to apply when they match.
`prepareProgram` copies that definition and validates its limits. Rules run in
registration order. Each rule pairs an event with commands or a Go callback.

| Event                 | Mutations                                                                             |
| --------------------- | ------------------------------------------------------------------------------------- |
| Element               | Set/remove attributes; before/after, replace/remove; prepend/append/set inner content |
| Text chunk or comment | Before/after, replace/remove                                                          |
| Explicit end tag      | Before/after, remove                                                                  |
| Document end          | Append                                                                                |

Insertion escapes text by default. HTML insertion requires an explicit flag.
Content operations reject void elements. Attribute values and text include entity
references; text chunks include context, chunk index and completion status.

`newSession` creates callback state for one HTML response. Callbacks receive
copied request metadata and parsed HTML events. Each session processes its events
serially and closes once. Separate responses can run concurrently.

Callbacks submit changes through a `mutations` builder using their invocation
context. A builder expires when its callback returns. Using it from another
callback fails that caller.

## Protocol

ABI version 1 uses little-endian integers and length-prefixed UTF-8 strings.
`sink.emit` writes output. `sink.exchange` copies an event to Go and accepts a
command reply while Rust owns the token. Rust validates stream, invocation, token, element, rule and event IDs before
applying a reply.

LOL HTML assigns element and scope IDs. A token observer gives overlapping
handlers the same token ID. An end-tag callback runs for the element closed by
that tag. Document end runs after successful EOF.

`programLimits` controls program, event, command, input and output sizes; parser
memory; event and command counts; and active scopes. `prepareProgram` sets the
execution timeout. `programRun` sets stream capacity. Guest initialization checks
protocol limits; Go checks imports, exports and ABI version. Callback code controls
its own memory and I/O.

## Shutdown and errors

Each stream gets a fresh instance. Errors, panics and exhausted limits stop it.
The caller closes an abandoned stream.

Shutdown stops admission and waits through its grace period, then cancels active
contexts. A blocked factory, handler or Close call keeps its slot and engine alive.
Shutdown reports unfinished work. The last exiting session closes the runtime;
subsequent shutdown calls return its cleanup result.

Go callbacks must cooperate with cancellation. Tests for callbacks that block
forever run in subprocesses that the parent kills.

## Tests

```sh
mise run //research/htmlrewrite:test
go -C research/htmlrewrite test -race -tags statute_htmlrewrite ./...
```

`TestProgram*` covers native/Wasm output and event comparisons, mutation rules,
text chunks, builder misuse, malformed messages, errors, cancellation, admission,
and cleanup. The subprocess tests cover stalled factory, handler and Close calls.

`create(buffered)` applies the `a.rewrite` and `.remove` fixture rules used by
HTTP tests and benchmarks. `create_program(length, buffered)` accepts a serialized
program.
