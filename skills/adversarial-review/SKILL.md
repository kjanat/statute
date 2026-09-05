---
name: adversarial-review
description: Review a Statute pull request from architecture boundaries outward and produce an approve/request-changes verdict.
disable-model-invocation: true
metadata:
  internal: true
---

# Adversarial review

Use this skill only when the user explicitly invokes `/adversarial-review` with a
Statute pull request number/URL.

Read `AGENTS.md` and `ARCHITECTURE.md`. Refresh the current PR head immediately.
Read the issue/goal, PR body architecture contract, complete diff, relevant
surrounding code, tests, current review threads, and CI.

Use the `statute-reviewer` role when subagents are available. Otherwise follow its
instructions directly.

Review the architecture before local code quality. Explicitly inspect:

- route vs pool/service/listener ownership,
- shared-state leakage,
- missing-policy fail-open behavior,
- Retry/re-entry/exactly-once assumptions,
- original vs rewritten request views,
- Docker generation retirement and pool reuse,
- Host/TLS parity between proxy and health traffic,
- TLS source selection and ACME lifecycle,
- partial startup, failed-start cleanup, retryability, sockets, server objects,
  goroutines, and transient state,
- resolved/runtime/export/graph/lint drift,
- cross-feature tests that would catch the plausible wrong implementation.

When the diff touches lifecycle/startup/shutdown code, run `make lint-lifecycle`
when the checkout is available. Inspect every `SLC100`–`SLC107` diagnostic against
the architecture contract rather than dismissing it as a style warning. The
analyzer is deliberately a second opinion, not a substitute for tracing runtime
ownership by hand.

For a new head after a previous review, compare from the previously reviewed SHA and
focus on the delta without reopening fixed findings unless they regressed.

Return concrete findings followed by one verdict: `APPROVE` or `REQUEST CHANGES`.
Do not post the review unless the user explicitly authorizes posting.
