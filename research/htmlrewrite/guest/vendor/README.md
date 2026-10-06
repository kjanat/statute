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

Four source files differ from the archive:

The complete reviewable delta is [end-token.patch](end-token.patch). Applying it
to the extracted archive reproduces those four files; the other imported files
are byte-for-byte upstream. Keep this patch synchronized when changing the fork.

- `src/selectors_vm/stack.rs`: report which popped entry actually owns the
  closing token. The first drained entry is explicit; descendants are implicit.
- `src/selectors_vm/mod.rs`: forward that identity to the rewrite controller.
- `src/rewriter/rewrite_controller.rs`: pass ownership to handler retirement.
- `src/rewriter/handlers_dispatcher.rs`: activate only the explicit owner's
  end handler; discard implicit descendants' handlers immediately. All matched
  content scopes and removal counters are still retired.

Original stack/VM entry points remain test-only wrappers for upstream unit tests.
There is no name comparison against mutated tags and no second HTML stack.
The patch also prevents automatic end-tag mutations (remove, rename, append)
for an implicit child from modifying its ancestor's token. It does not generate
synthetic closing tags or add browser DOM repair semantics.

`../tests/event_contract.rs` preserves the upstream reproduction.
`../tests/owned_end_contract.rs` tests the patched ownership and cleanup contract.
The Go regression exercises malformed input through both native and Wasm builds,
with explicit expected bytes rather than parity alone.

For upgrades, compare these four files against the pinned archive, port the
ownership change, rerun both contracts and the complete research suite, and
review parser-semantic differences before changing this pin. Remove the local
delta only when upstream satisfies the same regression tests.
