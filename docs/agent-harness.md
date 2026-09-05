# Agent development harness

Statute uses a small architecture-first harness for AI-assisted changes. The goal is
not more ceremony. The goal is to stop a locally reasonable PR from quietly moving
behavior onto the wrong shared object and making the next three PRs pay for it.

## Normative inputs

Every agent reads:

1. [`AGENTS.md`](../AGENTS.md) — workflow, stop conditions, and review matrix.
2. [`ARCHITECTURE.md`](../ARCHITECTURE.md) — stable ownership boundaries and runtime
   invariants.
3. The current issue/PR and current target branch — never stale chat memory.

`CLAUDE.md` points Claude-based runners at the same contract. `.claude/agents` and
`.agents/agents` both expose the shared [`agents/`](../agents/) role prompts; the
existing skill symlinks continue to expose [`skills/`](../skills/).

## Recommended flow

### 1. Freeze the change contract

Run:

```text
/architecture-contract #30
```

The architect is read-only. It identifies the owner, invariants, touched boundaries,
failure semantics, lifecycle/state ownership, required interaction tests, and any
decisions still needed.

If `Decisions required` is not `None`, resolve those decisions before code.

### 2. Implement

Use the existing `/next` workflow or the `statute-implementer` role. The implementer
must treat the accepted contract as fixed. If implementation reveals a missing
architecture decision, it stops instead of quietly changing the contract.

### 3. Open the PR with the contract visible

Use the default template or one of the specialized templates in
`.github/PULL_REQUEST_TEMPLATE/`:

- `lifecycle.md`
- `middleware.md`
- `routing-docker.md`
- `upstream.md`
- `tls-acme.md`
- `observability.md`

The PR body keeps the architecture contract beside the diff, where reviewers and
future agents can actually find it.

### 4. Review independently

Run:

```text
/adversarial-review <PR>
```

The reviewer verifies the contract against the current head and deliberately looks
for wrong-scope state, fail-open behavior, retry/re-entry errors, lifecycle leaks,
and resolved/runtime/tooling drift.

## Static lifecycle analyzer

`analyzers/statutelifecycle` complements the architecture review with repository-
specific `go/analysis` checks. It is built into the same custom golangci-lint binary
as `statutehttp` and can be run with:

```sh
make lint-lifecycle
```

Its first rules deliberately encode failures that are easy for a locally-correct
patch to miss:

- `SLC100` — a startup/bind function can publish serving and later return an error.
  This protects the two-phase rule: acquire every fallible startup resource before
  launching `Serve`.
- `SLC101` — a constructor calls `start`/`Start` on an object that also has a
  `stop`/`Shutdown`/`Close` lifecycle. Construction must not silently acquire
  background lifetime that failed `Start` cannot own.
- `SLC102` — a `net/http` or quic-go HTTP/3 `Serve*` error is discarded. A dead
  serving goroutine must not leave a bound endpoint advertised as healthy.
- `SLC103` — a paired `start`/`stop` lifecycle launches work its stop path does not
  visibly join. A `WaitGroup` launch owes a `Wait` on the exact same group,
  normalized to lifecycle owner root plus the complete field-selection path — so
  `r.a.wg` and `r.b.wg` are different groups, a wait on another owner's group
  proves nothing, and a launch through a group no owner root reaches, or whose
  provenance cannot be resolved, fails closed as undischargeable. Normalization is
  storage identity, not a lexical path: it refuses reassigned root variables,
  value-copy aliases (only pointer-typed aliases preserve storage identity),
  and any path whose prefix the body writes to or lets escape by address —
  directly or by passing a pointer alias onward as a value. The
  `Add(1)` + `go` + `defer Done()` shape spends explicit registration
  capacity: a constant positive `Add` whose statement dominates the launch in
  the block structure with no loop between them (a launch the runtime repeats
  spends capacity counted once; a `goto` disables registration for the whole
  body), one unit per launched literal whose first statement is its only
  `Done`, deferred, with no `goto` in the literal — the only shape proving
  exactly one `Done` per launch; a counter operation the model cannot
  account for — function literals included, a rejected launched literal's
  own `Done` among them — poisons that group's capacity, as does an
  accepted-shape launch that finds no capacity left to spend, whose own
  `Done` is then just as unaccounted, and everything
  else stays raw. Raw
  `go` launches stay deliberately count-based against visible channel receives.
  Channel identity is outside this rule's scope, making this conservative join
  evidence. Ambiguous cases produce a conservative diagnostic without claiming
  that a particular goroutine leaked.
- `SLC104`: lifecycle cleanup discards an error from `Close` or `Shutdown`.
- `SLC105`: a Docker `StartContainer` or `StopContainer` call escapes its canonical
  workload boundary, uses a detached or unbounded context, omits cancellation, or
  targets anything other than the operation binding through `workload.callRef`.
  Matching resolves the typed internal Docker client symbol independently of method
  spelling.
- `SLC106`: an owned stop attempt is reachable without a successful
  `persistOwnedStop` for the same provider, workload, and stop dominating that path.
  Persistence failure must return before Docker can receive the mutation.
- `SLC107`: durable mutation evidence or in-memory stop ownership is released
  outside canonical settlement, immutable binding supersession lacks a failed
  `sameContainerLocked` guard, or settlement lacks durable deletion, owner
  revalidation, generation fences, or reconcile scheduling.

The analyzer is initially disabled from the ordinary `make lint` set because current
`master` contains the lifecycle debt it was created to expose. Lifecycle PRs run the
targeted gate; once that baseline is clean, enable it by default instead of adding
baseline exclusions. Tests are excluded from repository lint diagnostics so
regression harnesses may intentionally construct broken lifecycle shapes; the
analyzer's own `analysistest` suite verifies those shapes are diagnosed.

Static analysis is not the architecture. It cannot prove external resource ownership,
ACME protocol state, Docker response timing, crash/restart recovery, route arbitration,
or whether a server really answered a request. A clean analyzer run therefore
supplements the lifecycle ownership table and behavioral cross-feature tests; it never
replaces them.

## Why the roles are separated

A model that chose an architecture is very good at later explaining why that choice
was sensible. That is not independent review. The role split makes architecture,
implementation, and verification separate evidence-producing passes, even when the
same underlying model is used.

## What belongs in architecture vs issue-specific contracts

`ARCHITECTURE.md` contains stable repository-wide boundaries. Do not fill it with the
acceptance criteria of every feature.

An issue-specific contract answers questions such as:

- Does `HealthCheck.Host` override `UpstreamHost`, or vice versa?
- Are passive failures windowed or consecutive?
- Is `FlushInterval` pool-only or route-specific?
- Is a failed `Start` retryable?

Those decisions belong in the issue/PR contract and become repository architecture
only when they establish a reusable boundary for later work.
