## What

<!-- Access log, metrics, tracing, profiling, or process-observability change. -->

## Why

<!-- Link the issue. -->

## Architecture contract

**Owner:** listener observability / auxiliary observability server

**Observed request view:** <!-- original route input / rewritten URL / final response; be explicit -->

**Invariants preserved:**

- [ ] Final response status is observed, not informational 1xx status
- [ ] Existing streaming/flushing behavior survives ResponseWriter wrapping
- [ ] Existing log/export shape changes only as explicitly documented
- [ ] Filtering, sampling, and error-always rules have explicit precedence
- [ ] Auxiliary observability listeners have explicit Start/rollback/Shutdown ownership

**Decisions required:** None

## ResponseWriter interfaces

<!-- If wrapping responses: Flush, ReaderFrom, Hijacker, Pusher, or other relevant optional interfaces. State which are preserved and why. -->

## Filter / sampling semantics

<!-- If applicable: deterministic gate order and examples such as whether Statuses("200-299") suppresses 500s. -->

## Cross-feature tests

- [ ] Informational response followed by final status
- [ ] Streaming / Flush propagation where applicable
- [ ] Efficient copy / ReaderFrom behavior where applicable
- [ ] Explicit filter vs sampling/error-always behavior
- [ ] Original vs rewritten request path/query semantics
- [ ] HTTP/1.1, HTTP/2, and HTTP/3 wrapper parity where applicable
- [ ] Failed-start + normal-shutdown ownership for auxiliary servers where applicable

## Validation

- [ ] `go test ./...`
- [ ] `go test -race ./...` / CI race job for concurrent writer/state changes
- [ ] `make lint`
- [ ] Observability docs/export/changelog updated
