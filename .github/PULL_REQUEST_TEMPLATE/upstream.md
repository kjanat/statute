## What

<!-- Pool/backend/transport/health change. -->

## Why

<!-- Link the issue. -->

## Architecture contract

**Owner:** upstream pool / backend state

**Scope:** <!-- Prove why this is pool-wide. If two routes sharing the pool may differ, stop and redesign. -->

**Invariants preserved:**

- [ ] Routes sharing one pool may differ only in route-owned policy
- [ ] Proxy traffic and active health checks share backend TLS verification policy
- [ ] Host policy precedence is explicit and consistent for proxy/probe traffic
- [ ] Backend health is backend state, not route state
- [ ] Existing degraded-mode behavior is intentionally preserved or explicitly changed

**Decisions required:** None

## Health semantics

<!-- If applicable: active/passive, consecutive vs windowed failures, recovery authority, retry-attempt vs final-response counting. -->

## Transport semantics

<!-- If applicable: connection/TLS/flush behavior and whether the value is pool-wide. -->

## Cross-feature tests

- [ ] Two routes sharing one pool
- [ ] Retry can select/observe multiple backend attempts correctly
- [ ] Health Host precedence
- [ ] Health/proxy TLS parity
- [ ] Passive demotion + active recovery where applicable
- [ ] Failure-window/consecutive semantics where applicable
- [ ] All-backends-unhealthy degraded mode
- [ ] Docker-created and static pools behave equivalently where applicable

## Validation

- [ ] `go test ./...`
- [ ] `go test -race ./...` / CI race job for mutable backend state
- [ ] `make lint`
- [ ] Resolved export/docs/changelog updated
