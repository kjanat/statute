# LOL HTML matcher patch

`lol_html/` contains the source, manifests, license, README and VCS provenance
from the crates.io `lol_html` 3.0.1 archive. Its SHA-256 is
`5adbb62638edf7e6bc88835cd3ea388bdd53382af42045da0e414ea78aa0c91a`.
The unmodified archive remains pinned as the `upstream_lol_html` development
dependency in `../Cargo.lock`; it is not linked into the guest or native oracle.
The BSD-3-Clause license is retained in `lol_html/LICENSE`.

Both the native oracle and Wasm guest use this local source. No build downloads
an unpinned fork or edits the Cargo registry. Keep upstream formatting intact;
this directory is excluded from the repository-wide formatter.

## Local delta

Six source files differ from the archive:

The complete reviewable delta is [end-token.patch](end-token.patch). Applying it
to the extracted archive reproduces those six files; the other imported files
are byte-for-byte upstream. Keep this patch synchronized when changing the fork.

- `src/selectors_vm/stack.rs`: report which popped entry actually owns the
  closing token. The first drained entry is explicit; descendants are implicit.
- `src/selectors_vm/mod.rs`: forward that identity to the rewrite controller.
- `src/rewriter/rewrite_controller.rs`: pass ownership to handler retirement.
- `src/rewriter/handlers_dispatcher.rs`: activate only the explicit owner's
  end handler; discard implicit descendants' handlers immediately. All matched
  content scopes and removal counters are still retired. Report selector matches
  and retirements to an optional parser-owned observer.
- `src/rewriter/settings.rs` and `src/lib.rs`: expose that private matcher
  observer. It reports element identity, registration index and content-scope
  eligibility; retirement reports explicit versus implicit closure.

The controller assigns identities before selector matching and stores matched
scope identities in the memory-accounted selector stack, including deferred
attribute matching. Void and foreign self-closing matches never replace their
parent's identity. The observer adds no second stack and no synthetic EOF events.
It runs before content callbacks and must not re-enter the same parser. IDs are
local to one parser; the future host ABI must add stream/invocation identities.

Original stack/VM entry points remain test-only wrappers for upstream unit tests.
There is no name comparison against mutated tags and no second HTML stack.
The patch also prevents automatic end-tag mutations (remove, rename, append)
for an implicit child from modifying its ancestor's token. It does not generate
synthetic closing tags or add browser DOM repair semantics.

`../tests/event_contract.rs` preserves the upstream reproduction.
`../tests/owned_end_contract.rs` tests the patched ownership and cleanup contract.
`../tests/matcher_context.rs` proves actual scope membership, including text-only
rules, overlap, implicit retirement, interleaved parsers and observer cleanup.
`TestMatcherContextNativeWasm` compares native and Wasm matcher/content traces
at different feed sizes and both output modes, with explicit retirement results.
The Go regression exercises malformed input through both native and Wasm builds,
with explicit expected bytes rather than parity alone.

## Updating

Renovate lists new LOL HTML releases in the Dependency Dashboard for approval.
It ignores the copied source's manifests. With clean pins, copied source and
provenance notes:

```sh
make -C research/htmlrewrite update-lol-html VERSION=3.0.1
make -C research/htmlrewrite test
```

Replace the example version with the desired release. The refresh command checks
the crates.io archive checksum, extracts the source, reapplies `end-token.patch`,
and resolves both pins and the lockfile in scratch space before replacing the
checkout's files. It also updates the version/checksum above. Node, Git, tar and
the pinned Rust toolchain are required; consumer builds never run this command.

Run `make -C research/htmlrewrite test-update-lol-html` to test the refresh tool
alone. This first fetches the complete locked dependency graph, including
target-specific packages that normal builds may not download. The fixtures then
run Cargo offline. The full `test` target includes this preparation and test.

A patch conflict stops the refresh without changing the checkout. Port the patch
against the new release in a separate scratch directory, save that patch, then
rerun the command. Review the source diff and parser-semantic changes even when
the patch applies cleanly. Upstream characterization may need updating if the
release fixes the original bug; retain Statute's ownership regressions. Remove
the local delta only when upstream satisfies those same regressions.

The reproduction and patch have been [submitted to upstream's existing issue](https://github.com/cloudflare/lol-html/issues/110#issuecomment-6027575856).
The matcher-context hook is an additional downstream extension; the linked
upstream comment proposes only the end-token ownership fix. No upstream
acceptance of either change is implied.
