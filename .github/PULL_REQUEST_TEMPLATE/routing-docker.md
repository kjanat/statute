## What

<!-- Route matcher/action or Docker discovery change. -->

## Why

<!-- Link the issue. -->

## Architecture contract

**Owner:** <!-- route / Docker router / Docker service; be precise -->

**Shared object:** <!-- Which pool/service/runtime state may multiple routes share? -->

**Invariants preserved:**

- [ ] Router-scoped policy remains router-scoped
- [ ] Service/pool state contains only behavior valid for every route sharing it
- [ ] Static routes retain precedence over dynamic Docker routes
- [ ] Route identity includes every semantic matcher/policy component, including order where order matters
- [ ] Unknown security/policy references fail closed at the affected router/route scope
- [ ] Sibling routers/services continue when only one dynamic object is invalid

**Decisions required:** None

## Route/action model

<!-- List the complete current mutually-exclusive route action set if changing actions. Do not copy a stale older list. -->

## Dynamic generation lifecycle

<!-- How are generations replaced/retired? Can new generation reuse any prior pool handlers safely? -->

## Cross-feature tests

- [ ] Two routers share one service/pool but have different route policy
- [ ] Policy ordering differences remain distinct where semantic
- [ ] Invalid router policy does not expose an unprotected route
- [ ] Invalid router does not unnecessarily kill valid siblings
- [ ] Static-vs-dynamic precedence
- [ ] Generation replacement/retirement where runtime state changes

## Validation

- [ ] `go test ./...`
- [ ] `make lint`
- [ ] Docker docs/labels and changelog updated where applicable
