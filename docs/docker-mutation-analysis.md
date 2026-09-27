# Docker mutation analysis

The lifecycle analyzer enforces the source boundaries behind Docker mutation
ownership. This is the coverage contract for
[issue #105](https://github.com/kjanat/statute/issues/105).
External lifecycle actors follow the
[single-writer observation contract](docker.md#external-lifecycle-actors)
defined by [issue #108](https://github.com/kjanat/statute/issues/108).
Cooperative pause/lease authority is not supported; an external Docker command
cannot revoke outstanding mutation ownership.

## Ownership contract

One `workloadStop` owns one immutable-container mutation across all its retry
attempts. An ambiguous attempt makes the owner
uncertain; a later rejected attempt cannot establish that the earlier stop did
not happen.

Fresh definitive rejection may settle. After ambiguity, settlement requires
positive stopped or missing-container evidence. Durable deletion must succeed
before ordinary admission resumes. Settlement revalidates the captured owner
after registry I/O, fences generations, and requests reconciliation.

Retirement revokes authority to issue new mutations. It does not revoke an
existing mutation's ownership, uncertainty, or quarantine. Container replacement
must preserve the predecessor independently and cannot retarget its Docker calls.

The analyzer owns no runtime resources. Its typed objects, provenance maps, and
control-flow state belong to one analysis pass. Diagnostics fail the affected
source boundary; they do not alter routing or configuration resolution.

## Cancellation contract

The original candidate in #105 requested `context.WithoutCancel`. The current
[architecture](../ARCHITECTURE.md#lifecycle) instead requires calls owned by the
provider run, with bounded timeouts and cancellation cleanup. A request does not
own a Docker mutation's lifetime.

Shutdown may cancel the provider's stop and inspect calls within its grace
period. Cancellation does not prove non-application: unresolved ownership remains
durable and non-serving. The next provider run resumes convergence before route
publication. Restoring `WithoutCancel` would contradict this shutdown contract.

`TestWorkloadStopConvergenceRestartsWithProviderRun` covers cancellation and
resumption. `TestWorkloadRejectedRetryDoesNotEraseStopUncertainty` covers a
rejected retry after ambiguity. The older non-cancellable wording in PR #104's
description predates the final architecture and is not the current contract.

## Scope of proof

These checks use typed fields, methods, constants, storage provenance, and control
flow. They protect mutation entry, state writes, and ownership release. Proofs
must distinguish a helper's identity from its implementation: a canonical name
alone is not evidence that arbitrary code inside the helper is safe.

Accepted syntax and trust boundaries are part of the analyzer contract.
Unsupported provenance at a protected boundary must produce a diagnostic.
Fixtures cover accepted refactors alongside
rejected bypasses, and diagnostics name the failed obligation.

Docker response timing, filesystem durability, scheduler interleavings, and
route arbitration are not simulated by the analyzer. Those remain integration,
property, and black-box test responsibilities. A successful audit does not prove
that a daemon response or external observation is truthful.

## Issue-to-rule coverage

| Original candidate                         | Enforcement and exact obligations                                                                                                                                                                                                                                                                          |
| ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Raw Docker mutation boundaries             | SLC105 restricts typed Docker start/stop calls to canonical owners, checks immutable destructive targets, and verifies `callRef`/`ref` helper bodies.                                                                                                                                                      |
| Persist before mutate                      | SLC106 requires a successful same-operation persistence gate before stop; helper-body checks require same-owner capture, valid durable record, successful insertion, post-I/O revalidation, and correctly ordered `persisted` writes.                                                                      |
| Bounded issued-mutation context            | SLC105/SLC106 require tracked provider-context provenance, bounded timeout, and cancellation cleanup. The refined shutdown contract above replaces the original unconditional `WithoutCancel` candidate.                                                                                                   |
| Monotonic uncertainty                      | SLC108 protects uncertainty, result, terminal eligibility, replay, and attempt/observation producers. Rejection cannot settle an owner with earlier ambiguity. Positive stopped/missing evidence can settle it.                                                                                            |
| Canonical settlement                       | SLC107 protects durable deletion, owner clearing/replacement, owner tuple and generation fencing; SLC108 validates terminal evidence; SLC109 prevents phase changes from bypassing those gates.                                                                                                            |
| Required publication/reconcile demand      | SLC107 requires generation fencing and reconciliation on asynchronous settlement paths, including normal early returns. Supersession is confined to the canonical observation binder inside reconcile; generation publication is covered by `TestWorkloadMutationQuarantineSurvivesServiceKeyReplacement`. |
| Immutable destructive targets              | SLC105 checks target provenance, immutable fallback capture before persistence, reference/allocator helpers, identity writes, and guarded observation/construction. SLC107 checks the same-container predicate before supersession.                                                                        |
| Grant retirement versus mutation ownership | Intentionally subsumed by SLC107, SLC108, and SLC109: protected evidence/owner/state sinks apply regardless of grant state. Registry retention preserves every unresolved owner and transfers the exact predecessor before replacement; pruning requires no stop under its mutex.                          |

The helper checks cover `callRef`, `ref`, `persistOwnedStop`,
`stopOwnershipLocked`, `currentLocked`, `stopResult`, and
`attemptOwnedStop`, `sameContainer`, `sameContainerLocked`, and the binding-key
allocator. Observation ingress and transition primitives are checked
alongside their callers. Typed aliases, parenthesized expressions, address
aliases, and equivalent early-return guards have focused fixtures. Unsupported
proofs at these protected boundaries are rejected; introducing a new abstraction
requires updating its proof and accepted/rejected fixtures.

This source-boundary proof assumes
ordinary typed Go without unsafe memory mutation, reflection-based private-state
writes, or replacement of trusted external library semantics. Docker error
classification itself is exercised by `internal/docker.TestLifecycleOutcomeAmbiguous`.
Actual persistence, process recovery, and event truth require the behavioral
tests below and the black-box Docker regression tier.

Incarnation keys assume the provider does not exhaust its finite-width counter.
Static checks reject counter resets, repeated allocator increments, and stale
pre-increment return aliases.

Exact published generation contents depend on the reconciled Docker snapshot,
candidate arbitration, and concurrent generation fencing. They are intentionally
behavioral test obligations. The static obligation is to retain the owner and
request reconciliation when asynchronous settlement changes that ownership;
`TestWorkloadMutationQuarantineSurvivesServiceKeyReplacement` verifies the
resulting route publication across predecessor/successor replacement.

`TestProductionLifecycleAudit` runs the complete analyzer over production
packages, including the e2e build-tagged entry point. Fixtures prove diagnostics;
this test prevents a clean fixture suite from concealing production findings.

## Behavioral evidence

New boundary regressions include
`TestWorkloadMutationOwnershipGuardsTransitions`,
`TestWorkloadCanonicalSettlementTransitions`,
`TestWorkloadStopConstructorPreservesExistingOwner`,
`TestRetiredMutationRetentionDoesNotDependOnRouteEligibility`, and
`TestPersistedStopFallbackKeepsPredecessorAfterBindingReplacement`.

| Boundary                                | Regression evidence                                                                                                 |
| --------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Persist before destructive call         | `TestWorkloadPersistsStopBeforeDockerMutation`                                                                      |
| Ambiguous stop never reopens traffic    | `TestWorkloadLostStopResponseNeverReopensServing`, `TestWorkloadStop500AfterSideEffectNeverReopensServing`          |
| Rejection before versus after ambiguity | `TestWorkloadFreshRejectedStopSettlesPreparedMutation`, `TestWorkloadRejectedRetryDoesNotEraseStopUncertainty`      |
| Registry I/O and generation ordering    | `TestWorkloadStopSettlementReleasesLockDuringRegistryDelete`, `TestWorkloadStopSettlementVersionsBeforeStateChange` |
| Retired ownership and republication     | `TestWorkloadRetiredIssuedStopQuarantinesSiblingRoutes`, `TestWorkloadRetiredStopSettlementRepublishesRoutes`       |
| Immutable predecessor versus successor  | `TestWorkloadContainerReplacementDoesNotInheritIssuedStop`, `TestRecoveredMutationDoesNotAttachToSameNameSuccessor` |
| Recovery after relabeling               | `TestRecoveredMutationSurvivesContainerRelabel`                                                                     |
| Shutdown cancellation and resumption    | `TestServerShutdownCancelsOutstandingDockerStop`, `TestWorkloadStopConvergenceRestartsWithProviderRun`              |

Run the complete production audit with `make audit-lifecycle`; run the incremental
PR gate with `make lint-lifecycle`. Neither permits production suppressions as a
substitute for proving ownership.
