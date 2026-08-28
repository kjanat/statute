---
name: next
description: Pick up the next actionable Statute tracking issue, or a specified issue, in an isolated Git worktree and carry it through a linked pull request.
disable-model-invocation: true
metadata:
  internal: true
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
commits, one branch push, and creation of one linked pull request carrying the
labels and milestone derived below. It also authorizes retiring the worktrees
and branches of pull requests GitHub reports as merged, and adding a `blockedBy`
relationship between tracked implementation issues when their current scopes
establish a genuine technical prerequisite. It does not authorize merging,
auto-merging, manually closing issues, changing repository settings, editing
unrelated issue content, removing existing dependency relationships, or
rewriting another contributor's branch.

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

1. Read [GitHub milestone commands](references/milestone-commands.md), then query
   GitHub for the current open milestones and their issues. The aliases are
   optional views; do not install or overwrite aliases unless the user asks.
2. Query #37 and all open repository issues.
3. Verify every open implementation issue is a child of #37. If an open issue
   has no parent, attach it with `gh issue edit ISSUE --parent 37` and verify the
   result. If it already has a different parent, stop and ask before reparenting.
4. Consider only open children of #37 with no open `blockedBy` issues and no
   open linked pull request.
5. Choose the best actionable issue using engineering judgment. Consider the
   issue's impact, readiness, milestone context, dependency-unblocking value,
   scope, risk, and fit with the current architecture and repository state.
   Milestone and issue numbers are context, not a required ordering. Explain
   the concrete selection rationale before implementation.
6. When the current issue scopes establish a genuine technical prerequisite,
   you may add the corresponding `blockedBy` relationship and verify it. State
   the technical reason. Never use dependency metadata merely to encode a
   preferred work order.
7. If no child is actionable, report whether the remaining children are
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
5. Perform every issue-related mutation from that worktree. Never repurpose an
   existing worktree, do not remove the new worktree at handoff, and remove an
   older one only under the merged-worktree cleanup below.

Use an issue-specific branch name derived from the issue number and title. Avoid
renaming an existing matching branch.

### Clean up merged worktrees

A run cannot retire its own worktree — its pull request is still open when the
run ends — so each run clears out what earlier runs left behind. Do this once,
before creating this run's worktree.

For every issue worktree and issue branch other than the one this run is about,
ask GitHub whether its pull request merged (`gh pr list --state all --json
number,state,headRefName,mergedAt`). Retire only those whose PR is `MERGED`:

```sh
git worktree remove <path>          # --force only for an empty, unmodified tree
git branch -D <branch>              # -d refuses after a squash merge
git worktree prune                  # clears metadata for a directory already gone
```

Leave everything else alone. A branch whose PR is open, closed unmerged, or
absent may hold unpushed work, and a worktree with uncommitted changes is
someone's work in progress: report those and move on rather than deleting them.
Never touch the primary checkout or a worktree this run did not create unless
GitHub says its pull request merged.

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

### Labels and milestone

Label every pull request you create. The canonical label set is
[`.github/labels.yml`](../../.github/labels.yml), and `gh label list` is the
live state. Read both, and apply only names that already exist live. Never
invent a label, and never run `gh label create` or `gh label edit`; the
label-sync workflow owns the label set. If a label you need is defined in
`.github/labels.yml` but is missing from the repository, apply the labels that
do exist and report the gap.

Derive the set from the selected issue and the change actually made:

- Carry over every `area: *` label the issue carries, plus `migration` and
  `security` when it carries them. Add an `area: *` label the issue lacks only
  when the change demonstrably touches that area.
- Apply exactly one kind label for the change: `bug` for a defect fix,
  `enhancement` for new or extended behavior, `documentation` for a
  documentation-only change.
- Do not copy triage labels onto a pull request. `good first issue`,
  `help wanted`, `question`, `duplicate`, `invalid`, and `wontfix` are issue
  triage; `dependencies`, `go`, and `github-actions` belong to the dependency
  bots.
- Apply `cr:skip` or `cr:review` only when the user explicitly asks for it.

Set the pull request's milestone to the selected issue's milestone, so the
milestone view shows the work in flight alongside its issue. An issue with no
milestone gives the pull request none; never create a milestone, and never move
the issue's own milestone.

Pass both to `gh pr create --label ... --milestone ...` so the pull request is
labeled and milestoned at creation. If creation rejects either, create the pull
request without it and attach the rest with `gh pr edit <number> --add-label`
and `gh pr edit <number> --milestone`. When `gh pr edit` fails on the
projects-classic deprecation error, fall back to the REST endpoints, which add
labels without replacing ones automation has already applied:

```sh
gh api -X POST "repos/{owner}/{repo}/issues/<number>/labels" -f 'labels[]=bug'
gh api -X PATCH "repos/{owner}/{repo}/issues/<number>" -F milestone=<milestone-number>
```

Do not relabel or remilestone the selected issue.

After creation, re-query GitHub and verify all of the following:

- The remote PR head SHA matches the pushed local head.
- The PR targets the default branch and is open.
- The selected issue lists the PR in linked/closing PR metadata.
- The selected issue remains a child of #37.
- Its blocking relationships match the initial state plus any genuine technical
  prerequisite added during this run; report and justify every graph mutation.
- The pull request carries the derived labels and the selected issue's
  milestone, and no repository label or milestone was created or edited.

Report the selected issue, selection rationale, blockers checked, worktrees
retired, worktree and branch, implementation, validations, commit and signature
status, push, applied labels and milestone, and PR URL separately. Leave this
run's worktree available for follow-up.
