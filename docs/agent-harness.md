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
