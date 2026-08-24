---
name: architecture-contract
description: Produce a read-only Statute architecture contract for an issue or proposed change before implementation.
disable-model-invocation: true
metadata:
  internal: true
---

# Architecture contract

Use this skill only when the user explicitly invokes `/architecture-contract`.
Accept an issue number/URL or a concise proposed change.

Read `AGENTS.md`, `ARCHITECTURE.md`, the current default branch, the complete issue
and substantive comments, linked PRs, and relevant code/tests/history.

Use the `statute-architect` role when subagents are available. Otherwise follow its
instructions directly.

This skill is read-only. Do not edit files, branches, issues, comments, or pull
requests.

Return a contract with exactly these headings:

- `## Current model`
- `## Owner`
- `## Invariants`
- `## Touched boundaries`
- `## Failure semantics`
- `## Lifecycle and state ownership`
- `## Required cross-feature tests`
- `## Decisions required`

The last section must say `None` before implementation can proceed without a human
design decision. Do not hide uncertainty in an implementation note.

Especially check the interaction matrix in `AGENTS.md`. For issues touching
routing/Docker, prove route-vs-pool/service scope. For middleware, prove ordering
and Retry behavior. For upstream health, prove Host/TLS parity and backend-attempt
semantics. For lifecycle, enumerate every acquired resource and retry-after-failure
state. For TLS/ACME, distinguish selected source, challenge availability, warm-up,
and manager ownership.
