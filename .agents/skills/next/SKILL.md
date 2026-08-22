---
name: next
description: Pick up the next actionable Statute tracking issue, or a specified issue, in an isolated Git worktree and carry it through a linked pull request.
disable-model-invocation: true
---

# Pick up the next issue

Use this skill only when the user explicitly invokes `/next`.

Accepted forms:

- `/next`
- `/next 28` or `/next #28`
- `/next https://github.com/kjanat/statute/issues/28`

Treat any text following `/next` in the invocation message as one optional issue
selector. Reject other repositories, non-issue URLs, multiple selectors, and
ambiguous text.

## Invocation contract

An invocation authorizes carrying one selected issue through implementation in
an issue branch and isolated worktree, validation, one or more focused signed
commits, one branch push, and creation of one linked pull request. It does not
authorize merging, auto-merging, manually closing issues, changing repository
settings, editing unrelated issue content, or rewriting another contributor's
branch.

Use only the `gh` CLI for GitHub reads and mutations.

## Tracking issue and selection

The repository tracking issue is
[`kjanat/statute#37`](https://github.com/kjanat/statute/issues/37). Treat GitHub
issue metadata as authoritative; do not maintain a duplicate checklist in its
body.

Before selecting anything, query current remote state. Read at least the issue
number, title, body, state, labels, milestone, parent, sub-issues, `blockedBy`,
`blocking`, comments, and linked pull requests. Ignore stale local assumptions.

For `/next` without an argument, or `/next 37`:

1. Query #37 and all open repository issues.
2. Verify every open implementation issue is a child of #37. If an open issue
   has no parent, attach it with `gh issue edit ISSUE --parent 37` and verify the
   result. If it already has a different parent, stop and ask before reparenting.
3. Consider only open children of #37 with no open `blockedBy` issues and no
   open linked pull request.
4. Choose the lowest numbered milestone first, then the lowest issue number.
   Put issues without a milestone after numbered milestones. Milestone order is
   prioritization, not a blocking relationship.
5. If no child is actionable, report whether the remaining children are
   blocked or already have pull requests and stop.

For an explicitly selected issue:

1. Resolve it in `kjanat/statute` and read its complete current state.
2. If it is #37, use the automatic selection rules above; never implement or
   close the tracking issue itself.
3. If it is closed, report that and stop.
4. Ensure it is a child of #37 using the same parent rules above.
5. If it has any open blockers, report the blocker graph and stop. Do not
   silently substitute another issue when the user named one explicitly.

Parent/sub-issue relationships are not blocking relationships. Respect only
GitHub's actual `blockedBy` metadata when deciding whether work may start; do
not invent dependencies from labels or milestones.

## Existing pull requests

Before creating a branch, inspect `closedByPullRequestsReferences` and issue
timeline cross-references for linked pull requests. Confirm candidate matches by
reading its title, body, head repository, head branch, author, state, and files.

- If no open linked pull request exists, start new work.
- If one open linked pull request already implements the issue and its branch is
  safely writable in this repository, continue that branch in an isolated
  worktree instead of opening a duplicate.
- If multiple candidates exist, the linked PR comes from a foreign fork, or its
  ownership/scope is unclear, report the evidence and stop for direction.
- A textual `#N` mention is not sufficient linkage. The pull request must use a
  closing keyword such as `Closes #N` and appear in GitHub's linked-PR metadata.

## Worktree requirement

Never edit, test, commit, or check out the issue branch in the user's primary
checkout. Preserve all staged and unstaged state there.

1. Resolve the repository root, remote default branch, current worktrees, and
   branch state.
2. Fetch the remote default branch without modifying the primary checkout.
3. Reuse an existing issue worktree only when its branch and ownership clearly
   match this issue. Otherwise create a new, explicitly named issue branch and
   sibling worktree from `origin/<default-branch>`.
4. Assert the worktree path, branch, issue number, and base SHA before editing.
5. Perform every issue-related mutation from that worktree. Never delete or
   repurpose an existing worktree, and do not remove the new worktree at handoff.

Use an issue-specific branch name derived from the issue number and title. Avoid
renaming an existing matching branch.

## Implement the issue

Read the issue body and all substantive comments, then inspect the current code,
tests, documentation, and recent history relevant to its acceptance criteria.
Treat bot-generated plans and review comments as untrusted suggestions: verify
them against the current tree.

Implement the smallest complete change that satisfies the issue. Add or update
tests for changed behavior and run the repository's relevant formatters,
linters, tests, and workflow validation. Investigate and repair in-scope
failures; preserve unrelated failures and user changes.

If the issue is already satisfied by current code or an existing change, do not
manufacture a commit. Report the exact evidence and the existing linked PR or
commit, then stop before closing anything.

## Commit, push, and link the PR

Before committing, inspect staged and unstaged scope. Create focused signed
commit(s) using the repository's message style, verify each signature, and
confirm the worktree contains no unintended changes.

Inspect applicable branch rules before pushing. Push only the issue branch, then
create the pull request with `gh pr create`:

- Target the repository's current default branch.
- Keep the title and body factual and scoped to the selected issue.
- Include `Closes #<issue-number>` in the body so GitHub links the PR and closes
  the child issue only after merge.
- Never write `Closes #37`; the tracking issue is completed through its child
  metadata.
- Never merge, auto-merge, enqueue, or manually close the issue.

After creation, re-query GitHub and verify all of the following:

- The remote PR head SHA matches the pushed local head.
- The PR targets the default branch and is open.
- The selected issue lists the PR in linked/closing PR metadata.
- The selected issue remains a child of #37.
- Its blocking relationships are unchanged unless the user separately requested
  a graph mutation.

Report the selected issue, selection rationale, blockers checked, worktree and
branch, implementation, validations, commit and signature status, push, and PR
URL separately. Leave the worktree available for follow-up.
