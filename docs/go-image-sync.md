# Go image mirror

The `sync Go images` workflow copies the official Go images from
`public.ecr.aws/docker/library/golang` to `ghcr.io/kjanat/golang` every day at
04:17 UTC. Maintainers can also run it manually on `master` through GitHub Actions.
It only publishes from `kjanat/statute` on `master`.

The language version and exact toolchain version in `go.mod` select the tags.
For Go 1.27 with toolchain 1.27.3, those are `1.27-bookworm`, `1.27-alpine`,
`1.27.3-bookworm` and `1.27.3-alpine`. Updating `go.mod` updates that selection;
older mirror tags remain available but stop receiving scheduled updates.

Each run resolves all source tags before copying, then copies each captured
digest with Skopeo's `--all --preserve-digests`. This retains every architecture
and the original manifest digest, even if a source tag moves during the run.
Skopeo reuses existing layers. Each destination tag is inspected afterwards;
a digest mismatch or registry error fails the job. Verified `FROM` references
appear in the job summary. Updates are per tag: if a later copy fails, earlier
verified tags remain updated. Rerunning the workflow completes the remaining work.

The workflow uses its `GITHUB_TOKEN` with `packages: write`. The existing `golang`
package must grant the `statute` repository write access under **Manage Actions
access** in its package settings. Package visibility and access are managed there;
the workflow does not change either. Credentials pass to Skopeo through standard
input, are stored in a temporary authentication directory, and are removed when
the script finishes. The hosted runner is discarded if the job is cancelled.

The workflow and script own only the image mirror. Dockerfile digest updates
continue through Renovate, and Statute releases use the existing release workflow.
The Skopeo container is itself selected by digest in the workflow.

Run `make test-go-image-sync`, `make typecheck` and `actionlint` to validate changes.
Tests cover source-tag selection, moving source tags, preservation of all
architectures, digest verification, registry failures and credential cleanup.
