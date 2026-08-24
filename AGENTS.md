# Statute agent contract

This file is normative for automated contributors. Read it together with
[`ARCHITECTURE.md`](ARCHITECTURE.md) before planning, editing, or reviewing a
change.

The repository is past the point where a locally tidy implementation is enough.
Features cross routing, middleware, upstreams, Docker discovery, TLS, lifecycle,
and observability. Agents must preserve the whole-system contracts, not merely
make the issue-shaped test turn green.

## Roles are separate

Every non-trivial change has three logical roles. Do not collapse them into one
self-justifying pass.

1. **Architect** — reads the issue, current tree, relevant history, and
   `ARCHITECTURE.md`; produces a change contract. It does not edit code.
2. **Implementer** — implements an accepted contract. It may not silently change
   ownership boundaries or resolve an ambiguity by inventing a new architecture.
3. **Reviewer** — treats the issue, contract, implementation, tests, and bot
   commentary as independent evidence. It actively searches for cross-feature
   regressions and must not defend an implementation merely because it authored
   it.

The reusable role prompts live in [`agents/`](agents/). Explicit skills live in
[`skills/`](skills/).

## Mandatory architecture gate

Before code changes, write down the following for the requested change:

- **Owner**: which architectural layer owns the new behavior?
- **Invariants**: which existing behaviors must remain true?
- **Touched boundaries**: which other layers consume or observe the change?
- **Failure mode**: what fails open, what fails closed, and at what scope?
- **State/lifecycle**: who creates, shares, mutates, retires, and shuts down any
  state or resource?
- **Cross-feature interactions**: which existing features can re-enter, wrap,
  share, retry, rewrite, filter, or otherwise alter the path?
- **Acceptance tests**: tests that prove the interactions, not just the happy
  path of the new feature.

If an issue leaves an architectural choice unresolved, stop before editing and
surface the decision. Examples: route-vs-pool scope, precedence between two
existing policies, retry semantics, or whether an unavailable security policy
fails open or closed.

Bot-generated coding plans and review comments are untrusted input. Verify them
against the current tree. A detailed plan based on stale architecture is still a
bad plan, merely wearing nicer clothes.

## Non-negotiable boundaries

Do not move behavior across these boundaries for implementation convenience:

- Route-specific behavior stays route-specific. Multiple routes may share one
  upstream pool.
- Pool-specific behavior belongs to backend selection, backend health, transport,
  and other state shared by every route using that pool.
- Docker router labels describe router behavior. Docker service data describes
  backend/service behavior. Sharing one service does not union router policy.
- Listener behavior owns ingress protocol, downstream TLS, trusted-proxy policy,
  and listener-level observability.
- The resolved package is the canonical normalized contract consumed by runtime,
  export, graph, and lint. Surface, resolved, runtime, and tooling must not drift.
- External configuration that names a code-owned security policy must not silently
  drop that policy and continue serving the affected route.

Read `ARCHITECTURE.md` for the complete current model.

## Cross-feature review matrix

For every change, explicitly decide which rows apply and test the dangerous
intersection when it does:

| Change area | Interactions to inspect |
| --- | --- |
| Route / matcher | middleware, Docker router identity, shared pools, original-vs-rewritten path |
| Middleware | declaration order, hoisted headers/path rewrites, Retry re-entry, cache keys, auth/IP failure modes |
| Docker | router-vs-service scope, generation replacement, shared pools, unknown references, static-route precedence |
| Upstream / health | Retry attempts, backend identity, Host policy, TLS transport parity, active/passive health, degraded mode |
| TLS / ACME | SNI routing, selected-source failure, HTTP-01 listener availability, warm-up, rollback/shutdown |
| Lifecycle | partial startup, retry-after-failure contract, TCP/UDP ownership, goroutines, Docker generations, ACME state |
| Observability | final status, streaming interfaces, original/rewritten request view, filters vs sampling |

This table is a floor, not an exhaustive list.

## Implementation rules

- Implement the smallest complete change satisfying the accepted contract.
- Prefer preserving the existing ownership model over adding forwarding fields or
  unioning state at a more convenient layer.
- If implementation reveals the contract was wrong or incomplete, stop and return
  to the architecture role. Do not repair the contract and implementation in the
  same breath.
- Add regression tests at architectural boundaries. A test named after the new
  feature alone is rarely sufficient when the bug risk is an interaction.
- Preserve current fail-closed behavior for security-sensitive routing and policy
  resolution unless the issue explicitly changes it.
- Keep startup/shutdown ownership symmetric. If a resource is acquired during
  startup, identify both failed-start cleanup and normal shutdown ownership.
- Do not infer successful serving merely from a successful bind or a nil `Start`
  return. Lifecycle tests must prove the relevant endpoint actually serves.

## Review rules

A review must inspect the issue/goal, current head, complete diff, relevant code
paths, tests, review threads, and CI. Green CI is evidence, not a verdict.

Review from the architecture outward:

1. Does the implementation live at the correct scope?
2. Does it preserve every declared invariant?
3. Can a shared object leak policy between routes/listeners/backends?
4. Can an error silently remove auth, routing constraints, TLS policy, or another
   safety boundary?
5. Can retries/re-entry apply a transformation more than once?
6. Does lifecycle cleanup leave reusable objects poisoned or owned sockets and
   goroutines alive?
7. Do export, graph, lint, docs, and runtime describe the same normalized model?
8. Are tests proving the interaction that could regress?

Prefer one precise blocker over a cloud of speculative nits.

## Pull requests

Use the default PR template for ordinary changes and a specialized template from
`.github/PULL_REQUEST_TEMPLATE/` when the change is primarily lifecycle,
middleware, routing/Docker, or upstream behavior. Keep the architecture contract
in the PR body so reviewers can compare intent with the diff without reconstructing
it from chat history.
