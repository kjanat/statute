# Vendored RFC sources

Statute keeps unmodified RFC Editor **HTML** snapshots in
[`docs/rfc/`](../docs/rfc/). They are offline research references. Open a snapshot
in a browser to navigate its section anchors, or search
its HTML with `rg`. External links and any external assets still require a network.

The initial corpus covers HTTP semantics (9110), caching (9111), HTTP/1.1 (9112),
HTTP/2 (9113), HTTP/3 (9114), and digest fields (9530). In particular:

- [RFC 9110 §12.5.3](https://www.rfc-editor.org/rfc/rfc9110.html#section-12.5.3):
  Accept-Encoding; local file `docs/rfc/rfc9110.html`, anchor `section-12.5.3`.
- [RFC 9110 §13.2](https://www.rfc-editor.org/rfc/rfc9110.html#section-13.2):
  precondition evaluation, in the same local file.
- [RFC 9111 §4.1](https://www.rfc-editor.org/rfc/rfc9111.html#section-4.1):
  cache selection using Vary; local file `docs/rfc/rfc9111.html`.

## Fetch and verify

From the repository root, using the existing Node tooling:

```sh
make vendor-rfcs                     # refresh all already-vendored RFCs
make vendor-rfcs RFCS="9110 9111"     # fetch or refresh selected RFCs
make check-rfcs                      # verify local bytes, without network access
make test-rfcs                       # test the tooling, without network access
```

`scripts/vendor-rfcs.mjs` resolves its default directory relative to itself.
`fetch` accepts only positive RFC numbers and uses
`https://www.rfc-editor.org/rfc/rfc<N>.html`. It applies a 30-second timeout and
10-MiB limit per document, rejects redirects and unexpected/incomplete HTML, and
downloads the complete selection before replacing any file. Fetch errors leave
the existing corpus untouched; there is no silent fallback. Files are replaced
individually by atomic rename, with the manifest last. An interruption during
publication can leave a mismatch, which the offline check rejects. Restore those
files from the reviewed Git snapshot before retrying.

The manifest records source URL, fetch time, byte count and SHA-256. Unchanged
downloads keep their original metadata, avoiding timestamp-only diffs. Review
changed HTML and its manifest together. Checksums identify the reviewed bytes;
they are not upstream signatures or proof that a document is still current.
Errata and later RFCs that update or obsolete these documents must be checked
separately when making protocol decisions.

CI checks the corpus and runs fixture-based tooling tests offline. Normal builds
never download RFCs. Formatters exclude the HTML so upstream bytes and notices
remain intact; Docker build contexts exclude the corpus. Wiki sync publishes this
guide and excludes the HTML documents.

Git also disables line-ending conversion for the snapshots, so verification works
with Windows checkouts as well as Unix checkouts. Both modern RFC HTML pages and
the RFC Editor's older annotated HTML fragments are supported.

## Provenance and rights

The fetch/provenance approach is adapted from
[micro509's RFC tooling at 2c85ad5](https://github.com/kjanat/micro509/blob/2c85ad5aac33597fe7350af413c31c6a9e076e55/scripts/fetch-spec.bun.ts)
and its
[resource provenance helper](https://github.com/kjanat/micro509/blob/2c85ad5aac33597fe7350af413c31c6a9e076e55/scripts/spec/resource.ts).
Statute implements this with Node built-ins and stores the HTML directly.

Each RFC retains its complete copyright and license notices. The vendored RFCs
remain subject to those notices and the referenced IETF Trust terms, **not**
Statute's software license. Snapshots are not reformatted, converted to text, or
presented as modified RFCs.
