# Go image mirror

The `sync Go images` workflow keeps the Go image mirror up to date daily at 04:17 UTC.
Scheduled runs discover tags from `public.ecr.aws/docker/library/golang`, falling
back to `docker.io/library/golang`, and copy them to `ghcr.io/kjanat/golang`.
New versions and variants are included automatically. Existing tags are refreshed
from their current upstream digest.

Manual dispatch on `master` accepts `tags` (commas or whitespace), `source`,
`destination` and `username`. Leaving `tags` empty discovers all source tags;
supplying tags limits that run to your selection. The defaults match the issue's
function's destination and username: `ghcr.io/kjanat/golang` and `kjanat`.
`source` accepts repositories in failover order, separated by commas or whitespace.
Use mirrors of the same image; discovery uses the first available tag listing.
Tags are validated against Docker's tag syntax, without a Go version or variant filter.

For local use with Docker, PowerShell and an authenticated `gh` CLI:

```powershell
. ./scripts/sync-go-images.ps1
Sync-GoImage
Sync-GoImage '1.27-bookworm', '1.27-alpine'
Sync-GoImage '1.27.3-bookworm', '1.27.3-alpine' -Destination 'ghcr.io/kjanat/golang'
```

Each tag's digest is calculated from the exact raw manifest bytes, so Windows-only
tags also resolve on a Linux runner. Copies use Skopeo's
`--all --preserve-digests --retry-times 3`. Discovery, digest resolution and copying
try the next source after a failure. Copy failover keeps the digest already chosen;
a mirror with different content cannot silently replace it. If every source fails,
the run stops; completed tags remain copied.
Successful copies print `FROM …@sha256:… AS builder`, also recorded
in the Actions summary. Dockerfile pins remain managed separately through Renovate.

The workflow publishes only from `kjanat/statute` on `master`. It supplies
`GITHUB_TOKEN` to `gh` with `packages: write`; the destination GHCR package must
grant this repository write access under **Manage Actions access**. Locally,
`gh auth token --hostname github.com` uses your own authentication. Credentials
reach Skopeo through stdin and a temporary auth file, removed when the function
finishes. Each run pulls `quay.io/skopeo/stable:latest` before using it.
