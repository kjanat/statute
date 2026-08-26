# Black-box e2e lane

The `e2e/` tree runs the compiled Statute binary across real network
boundaries: separate server, client, and origin processes in Docker
containers on a private network, in all four server/client topologies
(`1s1c`, `1s2c`, `2s1c`, `2s2c`). It complements the fast in-process
tests — it proves process boundaries, container networking, signal
handling, multi-instance isolation, and the deployed binary, none of
which `go test ./...` can.

## The boundary

The lane is black-box by contract, and the contract is mechanical:

- Every e2e package is denied `net/http/httptest` and
  `statute.kjanat.dev/internal/...` by a depguard rule in
  `.golangci.yml`.
- Only `e2e/cmd/statute` — the binary under test — may import the
  `statute` package at all. The harness, the actors, and the tests are
  held to a strict allow-list and observe Statute purely over the
  network and through artifacts.
- Readiness is only ever the health endpoint's `/healthz/ready`
  answering 200 over a real socket. A running container, a bound
  listener, or a nil process-launch result never counts.

## Layout

| Path                 | Role                                                                                                                     |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `e2e/Dockerfile`     | one digest-pinned image per commit holding `/statute`, `/origin`, `/client`                                              |
| `e2e/compose.yml`    | base stack: internal `mesh` network, `origin-1/2`, `statute-1`, `client-1`                                               |
| `e2e/topologies/`    | one override per topology adding `statute-2` / `client-2` with isolated state                                            |
| `e2e/scenarios/`     | scenario overrides and fixtures: Pebble (ACME), an OTel collector, the Docker-socket mount                               |
| `e2e/cmd/statute`    | the binary under test; compiles every scenario's config-as-code, selected by `STATUTE_SCENARIO`                          |
| `e2e/cmd/origin`     | behavior-rich upstream: echo with identity, health toggle, failure budgets, slow/streaming/upgrade endpoints, a journal  |
| `e2e/cmd/client`     | independent request driver: plan execution over HTTP/1.1/2/3, readiness waits, negative probes, streaming/upgrade checks |
| `e2e/harness`        | host-side orchestration: project naming, readiness, artifacts, teardown, the orphan proof                                |
| `e2e/report`         | the JSON plan/report schema between harness and client                                                                   |
| `e2e/testdata/certs` | throwaway checked-in PKI (see its README)                                                                                |
| `e2e/*_test.go`      | the scenarios themselves: `TestSmoke_*` (PR gate), `TestRegression_*` (deterministic), `TestSoak_*` (scheduled)          |

Everything carries the `e2e` build tag, so plain `go test ./...` never
compiles the lane and never needs Docker. Lint sees it because
`.golangci.yml` sets `run.build-tags: [e2e]`.

## Running locally

Prerequisites: a Docker daemon with Compose v2 and the `go` toolchain.
Nothing publishes a host port and images are digest-pinned, so runs are
parallel-safe and, after the images are pulled once, offline.

```sh
make test-e2e                 # the four-topology smoke matrix (PR gate)
make test-e2e-regression      # smoke + the deterministic regression set
make test-e2e-soak            # the stress/soak tier
make test-e2e E2E_REPEAT=20   # the orphan/collision audit
make e2e-clean                # recover from a killed run
```

One scenario, verbosely:

```sh
make e2e-image
cd e2e && STATUTE_E2E_IMAGE=statute-e2e:$(git -C .. rev-parse --short HEAD) \
  go test -tags e2e -run 'TestRegression_ACMEHTTP01' -timeout 15m -v .
```

## Artifacts

Every run writes `e2e/artifacts/<run-id>/<scenario>/<topology>/`:

- `compose.rendered.yml` — the fully resolved stack,
- `ps.json`, `logs.txt` — container states and all service logs,
- `reports/` — the plans the harness wrote and the structured reports
  the clients wrote back (this directory is the `/reports` bind mount).

CI uploads the whole directory when a job fails. The directory is
git-ignored.

## Cleanup contract

Teardown runs on success, failure, and timeout, in a fixed order:
diagnostics first (down would destroy their sources), then
`compose down -v --remove-orphans`, then the orphan proof — any
container, network, or volume of the run's project that survived fails
the test and is then force-removed. `TestMain` ends every invocation
with a lane-wide sweep over the `statute.e2e` label that fails the
suite if it reaps anything, and CI repeats that proof in an
`if: always()` step so even a cancelled job cannot leak silently. A
`docker kill`ed harness is recovered with `make e2e-clean`.

## Writing a scenario

1. Add a config builder in `e2e/cmd/statute` and register it in the
   `scenarios` map. Keep the conventional ports (`:8080` http, `:8443`
   https, `:8081` health) so the harness addresses every node the same
   way. Only this package imports `statute`.
2. If the scenario needs supporting services or node settings, add
   `e2e/scenarios/<name>/compose.yml` (digest-pinned images only) and
   pass it to `harness.Start`/`harness.StartServices` as an extra file.
3. Write the test: `harness.Start` (topology and scenario), then
   `AwaitReady`, then client plans via `ExecutePlan` and assertions
   over reports, origin journals (`/admin/requests`), service logs, and
   negative probes. Declare which topologies the scenario runs on by
   iterating them explicitly; assert edges, never aggregate counts.
4. Name it into a tier: `TestSmoke_*` runs on every PR and must stay
   fast; `TestRegression_*` runs nightly and on demand; `TestSoak_*`
   is the scheduled stress tier.

Failure semantics are strict on purpose: a missing prerequisite (no
`STATUTE_E2E_IMAGE`, no Docker) fails the test rather than skipping it,
so CI can never silently drop declared coverage.

## Known constraints

- **DNS-01 cannot be hermetic.** `dns01.go` is hard-wired to the
  Cloudflare API with no provider seam, so there is no local-DNS
  equivalent of the Pebble HTTP-01 scenario. The ACME scenario covers
  HTTP-01; DNS-01 remains covered by unit tests against the in-package
  fake.
- **ACME trust is two separate roots.** The Statute container trusts
  Pebble's static HTTPS root via `SSL_CERT_FILE` (which replaces the
  system bundle — fine, that scenario needs no other outbound TLS),
  while the client fetches Pebble's per-run issuance root from the
  management API before verifying the certificate Statute serves.
- **Retry(OnStatus) buffers.** Deciding to re-enter requires holding
  the response, which defeats streaming and hides `http.Hijacker` from
  upgrades. Streaming and upgrade scenarios run on Retry-free routes;
  deployments should shape their routes the same way.
- **Docker discovery uses the host daemon socket.** Discovered backend
  IPs must be routable from the Statute container, which containers
  inside a DinD daemon are not. Discovery stays opt-in per label, so
  the lane's own containers never surface as routes.
