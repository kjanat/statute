<!-- Thanks for contributing! A one-line summary of the change goes in the PR title. -->

## What

<!-- One short paragraph: what does this change do? -->

## Why

<!-- What problem does it solve? Link to the issue if applicable. -->

## Risk

<!-- Mark all that apply. -->

- [ ] Additive only — no existing API signatures change
- [ ] Touches `applyMiddleware` switch or `resolveMiddleware` type-switch (see below)
- [ ] Adds a new `MWxxx` iota constant (must be **appended**, never inserted)
- [ ] Touches the JSON export of `resolved.Config` (public contract)
- [ ] Changes process lifecycle (Start/Shutdown, signal handling)
- [ ] Adds a new external dependency

## Test plan

- [ ] `go test ./...` passes
- [ ] `golangci-lint run ./...` clean
- [ ] If new middleware: includes `${name}_test.go` covering happy path + at least one edge case
- [ ] If new CLI flag: mutual exclusion against existing flags verified
- [ ] If touching the access log: old log shape preserved (additive fields only)

## New middleware checklist (delete if not applicable)

- [ ] Surface type with `*xxxMW` pointer receiver implementing `statuteMiddleware()`
- [ ] `MWxxx` iota constant **appended** to `resolved.MiddlewareType`
- [ ] Fields added to `resolved.Middleware` (no reordering existing fields)
- [ ] `case *xxxMW:` arm in `resolveMiddleware`
- [ ] `case resolved.MWxxx:` arm in `applyMiddleware`
- [ ] `xxxHandler(m resolved.Middleware, next http.Handler) http.Handler` implementation
- [ ] `${name}_test.go` with table-driven cases
- [ ] godoc on every exported symbol
