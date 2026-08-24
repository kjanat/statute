## What

<!-- Lifecycle/startup/shutdown change. -->

## Why

<!-- Link the issue or failure. -->

## Architecture contract

**Owner:** lifecycle / server runtime

**Startup commit point:** <!-- When is the server considered successfully started? -->

**Failed-Start retry contract:** <!-- retryable / deliberately single-shot; explain -->

**Invariants preserved:**

- [ ] No application serving is published while Start can still fail synchronously
- [ ] A failed start leaves no owned serving socket or background loop alive
- [ ] Normal shutdown owns every resource committed by Start
- [ ] Cleanup ordering preserves HTTP-01/ACME requirements where applicable
- [ ] A retry cannot reuse poisoned server/control objects or retired runtime state

**Decisions required:** None

## Resource ownership table

| Resource                                                                        | Constructed | Acquired/started | Failed-start cleanup | Normal shutdown | Retry state reset |
| ------------------------------------------------------------------------------- | ----------- | ---------------- | -------------------- | --------------- | ----------------- |
| <!-- TCP listener / UDP conn / ACME manager / Docker / health server / etc. --> |             |                  |                      |                 |                   |

## Concurrency / goroutines

<!-- Which goroutines start, how are they cancelled, and who waits for them? -->

## Cross-feature tests

- [ ] Failure after at least one earlier resource was acquired releases it
- [ ] Failed Start never exposes an application route, including on an accepted keep-alive/H2 connection
- [ ] Retry after fixing the external failure actually serves, not merely returns nil
- [ ] TCP listener ownership checked where applicable
- [ ] HTTP/3 UDP `PacketConn` ownership and a real HTTP/3 request checked where applicable
- [ ] Unexpected `Serve` exit is observed and retires caller-owned resources
- [ ] ACME warm-up/cooldown/transient state checked with real in-flight cancellation where applicable
- [ ] Docker dynamic generation/pool retirement checked where applicable
- [ ] Normal `Shutdown` still releases all committed resources

## Validation

- [ ] `go test ./...`
- [ ] `go test -race ./...` / CI race job
- [ ] `make lint`
- [ ] `make lint-lifecycle`
- [ ] `CHANGELOG.md` updated for user-visible lifecycle behavior
