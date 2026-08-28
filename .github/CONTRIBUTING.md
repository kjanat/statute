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

That should work on any platform with Go 1.27+. The Makefile is the canonical interface for development tasks:

```sh
make test            # all unit tests
make test-race       # tests with -race (Linux/macOS x86_64 only — see below)
make fmt-check       # Go source form and import grouping
make lint            # golangci-lint
make cover           # write coverage report + HTML
make bench           # microbenchmarks
make fuzz            # 30s per fuzz target
make build-examples  # compile all examples
make test-e2e        # black-box smoke matrix in Docker (see docs/e2e.md)
make test-e2e-regression  # smoke + deterministic regression scenarios (PR gate)
make test-e2e-soak   # e2e stress/soak tier
make e2e-clean       # remove anything a killed e2e run leaked
make all             # everything fast (the e2e lane stays separate)
```

### A note on `-race`

Go's race detector (and TSAN underneath) does not work on Raspberry Pi or older ARM kernels with VMA range < 48 bits. If `make test-race` fails on your local machine with `FATAL: ThreadSanitizer: unsupported VMA range`, that's the kernel, not your code. CI runs `-race` on Linux x86_64 and will catch races there.

## Workflow

1. Open an issue. We'll discuss scope before code lands.
2. Fork and branch from `master`.
3. Make the change. Every PR must:
   - Add tests where applicable. Coverage should not decrease meaningfully.
   - Pass `make fmt-check` and `make lint`. CI fails closed on both.
   - Update `CHANGELOG.md` under the `## [Unreleased]` heading with a brief entry.
   - Update godoc on any exported symbol you add or change.
4. Open a PR. Fill in the template; mark the new-middleware checklist if applicable.

## Dependency updates

Renovate owns dependency updates for this repository. The configuration lives in [`renovate.json`](../renovate.json) at the repository root, next to the other tool configs. Dependabot's config is gone; its _security alerts_ are a repository setting rather than a file, so those keep arriving independently.

Renovate covers the Go module, the npm devDependencies and their lockfile, GitHub Actions, and the container images in `e2e/` and `examples/`. Custom regex managers in the same file pick up the version strings that do not live in a manifest at all: the golangci-lint release in `.custom-gcl.yml` and in the mise block of `ci.yml`, and the `kjanat/gpg-signing-service` release the `sign-commits` composite action installs.

Two couplings are worth knowing about before you edit any of it by hand:

- **golangci-lint is bumped in four places at once.** `.custom-gcl.yml` builds the custom linter binary that carries this repo's own analyzers, and `.github/workflows/ci.yml` installs golangci-lint through mise and passes a version to `golangci-lint-action` twice. If those drift, the analyzers run against a different linter than the one CI installs, and the mismatch is silent. Renovate groups all four into a single PR. Keep them together if you edit them by hand.
- **Images in `e2e/` are pinned as `image:tag@sha256:digest`.** The digest is what makes a run reproducible; the tag is what makes the pin readable and trackable. A bare `image@sha256:...` has no tag to anchor it, so it resolves against `latest`, and the next "digest refresh" silently swaps the image for something else entirely. Example images under `examples/` deliberately stay on plain tags, because they are documentation.

Grouping mostly follows what Dependabot did, with one deliberate divergence: Renovate knows opentelemetry-go and opentelemetry-go-contrib as two separate monorepos, so the core `v1.x` modules and the `v0.x` contrib ones (`otelhttp`) now bump independently instead of being unioned into one `otel` group. That is the better shape here, because it stops a core release waiting on an unrelated contrib release.

Renovate opens a dependency dashboard issue listing everything it knows about, including updates it is holding back. The `go` directive in `go.mod` is one of those: raising it raises the minimum Go a consumer needs, so it waits for a human to tick the box on the dashboard rather than arriving as a surprise PR.

Nothing automerges. Renovate PRs go through the same review as every other change.

There is no CI job validating `renovate.json`; Renovate reports its own config errors on the dashboard and in its PRs. If you want to check a config change locally before pushing, two traps are worth knowing, because both fail in a way that looks like a real answer:

```sh
npx --yes --package renovate@44.46.0 renovate-config-validator --strict renovate.json
```

- Keep the `--package renovate@<version>` form. Bare `renovate-config-validator` is a _different_ npm package, a third-party placeholder with no executable, so `npx renovate-config-validator` never runs the real tool.
- Keep the version pinned. Renovate 44 declares `engines.node: ^24.11.0`, so on a newer Node an unpinned install silently walks back to 37.440.7, whose validator reports false errors for `platformCommit`, `managerFilePatterns` and `github-runners`.

Note what the validator does not do: it checks syntax, unknown keys and malformed regexes, but it exits 0 on a `packageRules` entry that matches nothing at all. A rule aimed at a manager or datasource that does not exist is accepted in silence, so if a rule is meant to disable or group something, confirm it by running an extraction rather than by a clean validator run.

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

- Run `make fmt-check` before committing. It checks Go source form with toolchain `gofmt` and import grouping with the pinned `go tool goimports`.
- Imports are grouped as stdlib, then external deps with a blank line, then `statute.kjanat.dev` last.
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
