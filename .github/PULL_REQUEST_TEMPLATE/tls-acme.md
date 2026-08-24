## What

<!-- Downstream TLS, certificate routing, or ACME change. -->

## Why

<!-- Link the issue or interoperability/security requirement. -->

## Architecture contract

**Owner:** listener TLS policy / certificate source / ACME manager

**Selected-source behavior:** <!-- What happens after SNI selects a source and that source errors? -->

**Challenge availability:** <!-- HTTP-01 / DNS-01 / automatic; what listener/external dependency is required? -->

**Invariants preserved:**

- [ ] Exact SNI beats wildcard; wildcard beats hostless fallback
- [ ] A selected source failure does not silently fall through to a different certificate policy
- [ ] Automatic and pinned ACME manager ownership remains distinct
- [ ] HTTP-01 work runs only while its plain-HTTP challenge path can answer
- [ ] TCP and HTTP/3 use compatible listener TLS policy and certificate routing
- [ ] TLS policy cannot advertise a protocol/cipher/challenge combination the selected source cannot satisfy

**Decisions required:** None

## ACME lifecycle / storage

<!-- Manager start, warm-up timing, cancellation, failed-order cooldown, persistence, retry, shutdown ordering. N/A for static-only changes. -->

## Cross-feature tests

- [ ] Exact + wildcard + fallback source selection where applicable
- [ ] Selected source error does not escape to fallback
- [ ] HTTP-01 challenge path availability during warm-up/shutdown/rollback
- [ ] DNS-01 propagation/order timeout behavior where applicable
- [ ] Failed-start/retry does not retain poisoned ACME transient state
- [ ] TCP and HTTP/3 certificate behavior
- [ ] TLS version/cipher compatibility with automatic and pinned source key types
- [ ] Duplicate-domain/storage/account interactions where applicable

## Validation

- [ ] `go test ./...`
- [ ] `go test -race ./...` / CI race job for manager lifecycle changes
- [ ] `make lint`
- [ ] TLS/ACME docs, lint rules, resolved export, and changelog updated where applicable
