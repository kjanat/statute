# Contributing to statute

Thanks for considering a contribution. This document covers what we expect from a PR and how to set up a development environment.

## Before you start

statute is small and opinionated. Before opening a PR for a new feature, please open an issue describing the use case. Some categories are explicit non-goals:

- Dynamic configuration reload, runtime config files, plugin loading.
- Web admin UIs.
- Scripting languages (Lua, Starlark, etc.).
- Generic L4 (TCP/UDP) load balancing — statute is L7-first. SNI-based TLS passthrough and PROXY-protocol ingress may be considered, since they compose with the existing listener/TLS path; open an issue first.

If your idea is in one of these categories, [Caddy](https://caddyserver.com/) or [Traefik](https://traefik.io/) is a better home for it.

## Development environment

```sh
git clone https://github.com/kjanat/statute.git
cd statute
go test ./...
```

That should work on any platform with Go 1.25+. The Makefile is the canonical interface for development tasks:

```sh
make test            # all unit tests
make test-race       # tests with -race (Linux/macOS x86_64 only — see below)
make lint            # golangci-lint
make cover           # write coverage report + HTML
make bench           # microbenchmarks
make fuzz            # 30s per fuzz target
make build-examples  # compile all examples
make all             # everything
```

### A note on `-race`

Go's race detector (and TSAN underneath) does not work on Raspberry Pi or older ARM kernels with VMA range < 48 bits. If `make test-race` fails on your local machine with `FATAL: ThreadSanitizer: unsupported VMA range`, that's the kernel, not your code. CI runs `-race` on Linux x86_64 and will catch races there.

## Workflow

1. Open an issue. We'll discuss scope before code lands.
2. Fork and branch from `master`.
3. Make the change. Every PR must:
   - Add tests where applicable. Coverage should not decrease meaningfully.
   - Pass `make lint`. The CI fails closed on lint.
   - Update `CHANGELOG.md` under the `## [Unreleased]` heading with a brief entry.
   - Update godoc on any exported symbol you add or change.
4. Open a PR. Fill in the template; mark the new-middleware checklist if applicable.

## Adding new middleware

The framework has a strict template for middleware to keep the codebase navigable:

1. **Surface type**: an unexported pointer type `*fooMW` in `<name>.go` that implements the `Middleware` marker via `statuteMiddleware()`.
2. **Constructor + chainable setters**: `Foo(...)` returns `*fooMW`; chained methods return `*fooMW`.
3. **Resolved entry**:
   - Append a new `MWFoo` constant to the iota in `resolved/resolved.go`. **Never insert** mid-iota — the JSON export of the resolved config is a public contract.
   - Add fields to `resolved.Middleware` for any data the resolved form needs.
4. **Resolve arm**: a `case *fooMW:` in `resolveMiddleware` (`resolve.go`).
5. **Apply arm**: a `case resolved.MWFoo:` in `applyMiddleware` (`server.go`).
6. **Handler**: implement `fooHandler(m resolved.Middleware, next http.Handler) http.Handler` in `<name>.go`.
7. **Tests**: a sibling `<name>_test.go` that uses the helpers in `testutil_test.go` (echo backend, `mustResolve`, `runRequest`, `assertHeader`).

If the middleware changes the access-log shape, the change must be **additive** — guard new fields on a non-nil context value so old log consumers aren't surprised.

## Style

- `gofumpt` formatted (the linter enforces this). Run `gofumpt -w .` if your editor doesn't.
- Imports grouped: stdlib, then external deps with a blank line, then `statute.kjanat.dev` last. The linter's `goimports` config enforces the `statute.kjanat.dev` local-prefix.
- No comments stating _what_ code does — names should be self-explanatory. Comments should answer _why_: non-obvious constraints, hidden invariants, historical context.
- No `// TODO` or `// FIXME` comments in committed code. If something is genuinely deferred, open an issue and reference it from a code comment.
- Errors include a path-style context: `fmt.Errorf("upstream %q: %w", name, err)`.

## Versioning

statute follows [Semantic Versioning](https://semver.org/) and is pre-1.0
(minor releases may include breaking surface-API changes). The version is
**the git tag** (`vX.Y.Z`) plus a matching `CHANGELOG.md` section — there
is deliberately no `VERSION` file or in-source version constant. Go
modules have no version field in `go.mod`; the module version _is_ the
tag, and `debug.ReadBuildInfo()` surfaces it at runtime. PRs do not bump
a version; releases are cut by tagging.

## Reporting security issues

Don't open a public issue. See [SECURITY.md](SECURITY.md).
