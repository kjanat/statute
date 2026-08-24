<!-- Thanks for contributing. Keep this body useful to someone reviewing the architecture, not just the diff. Specialized templates live in .github/PULL_REQUEST_TEMPLATE/. -->

## What

<!-- One short paragraph: what does this change do? -->

## Why

<!-- What problem does it solve? Link the issue if applicable. -->

## Architecture contract

**Owner:** <!-- route / pool / listener / Docker router / Docker service / resolved/tooling / other -->

**Invariants preserved:**

- <!-- Existing behavior that must remain true. -->

**Touched boundaries:**

- <!-- Retry, Docker sharing, TLS/ACME, lifecycle, observability, export, etc. -->

**Failure semantics:** <!-- What fails open/closed, and at what scope? -->

**Lifecycle/state ownership:** <!-- Who creates, shares, retires, rolls back, and shuts down state/resources? N/A if none. -->

**Decisions required:** None

## Cross-feature tests

<!-- Name the interaction tests, not only the new feature's happy path. -->

- [ ] Shared-scope behavior checked where applicable
- [ ] Retry/re-entry behavior checked where applicable
- [ ] Fail-open/fail-closed behavior checked where applicable
- [ ] Resolved/runtime/export/tooling stay aligned where applicable
- [ ] Lifecycle rollback + normal shutdown checked where applicable

## Risk

- [ ] Additive only — no existing API signatures change
- [ ] Changes route/matcher/action semantics
- [ ] Touches middleware ordering, hoisting, or `applyMiddleware`
- [ ] Adds a new `MWxxx` iota constant (append only, never insert)
- [ ] Touches Docker route/service mapping or dynamic generations
- [ ] Touches upstream pool, health, Host, or transport behavior
- [ ] Touches downstream TLS/ACME policy or certificate selection
- [ ] Touches the JSON export of `resolved.Config` (public contract)
- [ ] Changes process lifecycle (`Start`/rollback/`Shutdown`)
- [ ] Touches access-log/metrics response-writer behavior
- [ ] Adds a new external dependency

## Test plan

- [ ] `go test ./...` passes
- [ ] `make lint` clean
- [ ] Relevant cross-feature regression tests added
- [ ] Documentation/godoc updated for changed public behavior
- [ ] `CHANGELOG.md` updated when the change is user-visible
