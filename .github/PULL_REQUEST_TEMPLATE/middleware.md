## What

<!-- Middleware behavior being added/changed. -->

## Why

<!-- Link the issue. -->

## Architecture contract

**Owner:** route middleware

**Placement:** <!-- ordinary wrapper / hoisted request edge / hoisted response edge -->

**Invariants preserved:**

- [ ] Declaration order remains semantic
- [ ] Exactly-once operations stay outside Retry re-entry
- [ ] Route matching still observes the intended original/transformed request view
- [ ] Cache/upstream/remaining middleware observe the intended transformed view
- [ ] Security-sensitive policy cannot disappear and leave a route serving

**Decisions required:** None

## Resolved model

- [ ] Surface type/builder updated
- [ ] `MWxxx` enum appended, never inserted, if applicable
- [ ] `resolved.Middleware` carries normalized data
- [ ] Resolve validates/normalizes once
- [ ] Runtime consumes only resolved semantics
- [ ] Export/docs reflect the same semantics

## Cross-feature tests

- [ ] Middleware order
- [ ] Retry/re-entry
- [ ] Request clone / URL clone when mutation is involved
- [ ] Original vs rewritten path/query where applicable
- [ ] Docker named-middleware mapping where applicable
- [ ] Missing external middleware reference remains fail-closed
- [ ] Streaming/ResponseWriter interfaces preserved for response wrappers

## Validation

- [ ] `go test ./...`
- [ ] `make lint`
- [ ] New/changed middleware has happy-path and interaction regression tests
- [ ] Godoc/docs/changelog updated
