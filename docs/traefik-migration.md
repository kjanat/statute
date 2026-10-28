# Offline Traefik migration audit

The migration command inventories Traefik configuration and writes a Go skeleton
plus a JSON audit. It does not start a proxy, contact Docker, acquire certificates,
interpolate environment variables, or perform a cutover. Traefik files do not
become a Statute runtime input format.

From a checkout:

```sh
go run ./cmd/statute --help
go run ./cmd/statute migrate --help
go run ./cmd/statute migrate traefik --help
```

Help is available at every command level with `--help` or `-h`. Running `statute`
without arguments shows the command list. Help exits successfully without reading
configuration or creating files.

To audit a configuration:

```sh
mkdir -p /tmp/statute-migration
go run ./cmd/statute migrate traefik \
  --static traefik.yml \
  --dynamic dynamic/ \
  --compose compose.yml \
  --out /tmp/statute-migration/main.go \
  --report /tmp/statute-migration/migration.json
```

Alternatively, `go install ./cmd/statute` installs the tooling command. It is
separate from the `statute.Main(config)` flags embedded in proxy applications.
Generate `main.go` in an application or temporary directory, outside the library's
package directory.

Each input option is repeatable. Each occurrence supplies one literal path,
including paths containing commas or dollar signs; arguments are not split into additional paths or expanded
from environment variables. Static and file-provider sources support YAML
and TOML. Dynamic directories are traversed in sorted order for `.yml`, `.yaml`,
and `.toml` files. Compose labels support mapping and list syntax. Supply every
file explicitly: the command does not follow provider paths, resolve Compose
includes/overrides, inspect images for exposed ports, or query running containers.
Includes, inheritance and Swarm settings require manual review. Duplicate
provider-qualified definitions block acceptance rather than being merged.

Output files must be new, distinct paths. They are created with mode `0600` and
never overwrite existing files. Parsing/writing failures remove newly reserved
outputs. Inputs are bounded to 256 files of at most 8 MiB each. Directory symlinks
and multiple YAML documents in one file are rejected.

## Reading the audit

The human report goes to stdout. JSON schema version `1` includes source paths,
provider-qualified identities, every inventoried field's classification,
reference names, source router and distinct host counts, rules with unknown
counts, generated route counts, unmatched objects, and canonical-tool checks.
Compose inventory covers Traefik labels and mechanisms that could alter them,
not unrelated application images, environment values, or labels.

| Classification | Meaning                                                                                                      |
| -------------- | ------------------------------------------------------------------------------------------------------------ |
| `exact`        | A supported source setting retains its existing semantics.                                                   |
| `translated`   | An explicit native representation or retained label implements the classified setting. Read its explanation. |
| `manual`       | A decision, missing input, secret binding, or semantic difference prevents automatic acceptance.             |
| `unsupported`  | The parser or implementation cannot represent the behavior.                                                  |
| `out_of_scope` | The object is intentionally outside this HTTP tool, such as the Traefik dashboard.                           |

An object's classification reflects its most restrictive field. `generated`
means it contributed a fragment or retained Docker setting, not that every field
passed. TLS paths, for example, can contribute an HTTPS candidate while coverage
remains blocking. `unmatched_objects` lists objects without a contribution.
Disjunctions and host spelling expansion can produce several native routes from
one router. Docker counts describe parsed labels, not live discovery.

| Exit status | Meaning                                                       |
| ----------- | ------------------------------------------------------------- |
| `0`         | Help displayed, or no blocking findings or validation errors. |
| `1`         | Artifacts were written, but migration gaps remain.            |
| `2`         | Invalid invocation/input or artifact I/O failure.             |

Blocking findings cause generated `main` to panic before invoking Statute.
`configuration()` remains available for review and compilation. Complete the
configuration and resolve every blocking finding before replacing the guard with
`statute.Main(configuration())`.

## Initial translation coverage

- Explicit plaintext addresses become HTTP listeners. A single certificate/key
  pair can produce an HTTPS candidate for a TLS-enabled entrypoint. SNI coverage,
  TLS policy, resolver selection, challenge availability and storage remain manual.
- Ordinary HTTP/HTTPS file-provider services become named pools. Host forwarding
  and supported named transport fields become pool-owned policy. Credential-bearing
  URLs, queries, base-path rewriting and unrecognized service fields block acceptance.
- The existing Traefik parser inventories `Host`, `Path`, `PathPrefix`, conjunctions
  and disjunctions. Native routes represent exact paths, catch-all paths and finite
  Traefik host spellings, including trailing-dot variants. Literal paths ending in
  `/*` are refused because native wildcard matching would broaden them.
- `PathPrefix('/api')` includes `/api-secret`; native `Match('/api/*')` is
  segment-aware. Non-root file-provider prefixes require manual work. Docker labels
  retain their existing byte-prefix implementation.
- File routes follow explicit priority or rule length. Equal-priority overlaps,
  overlapping file/Docker routes, and Docker specificity-versus-priority differences
  block acceptance. Catch-all candidates become fallback routes; moving a
  higher-priority catch-all to fallback is reported as a behavior change.
- Explicit Docker settings produce `Docker().TraefikLabels()`. Middleware
  registrations use the exact label reference names; Host policy uses exact-service
  `PoolPolicy` keys. Implicit services, missing ports and missing explicit container
  opt-in require review. Compose cannot prove the live topology.
- IP allow lists without IP strategies, response-header removals and supported
  middleware chains become compiled policy. References stay provider-qualified and
  ordered. Routers sharing a pool retain distinct middleware. Missing/cyclic chains
  block acceptance; expansion is bounded to 64 levels and 256 operations per chain.
- Simple redirect-scheme declarations produce guarded redirect-action candidates;
  forwarded-scheme trust and listener reachability remain manual.
- Auth, nonempty header values, buffering, retry, compression and path rewrites
  require review. Statute hoists header/path rewrites and has explicit buffering
  budgets and streaming behavior; similar middleware names do not establish parity.
- Entrypoint restrictions and router TLS settings block acceptance because native
  content listeners share a route table. Unsupported matchers, plugins, TCP/UDP
  traffic and external providers never silently become HTTP routes.

Unrecognized fields default to `manual`; they are not discarded to make validation
pass. See [Docker compatibility](docker.md#traefik-compatibility-traefiklabels) for
the runtime label subset and its refusal behavior.

## Secrets and validation

Reports omit source values. Environment names such as `env:AUTH_TOKEN` and known
file references are retained without evaluation; `${TOKEN:-inline-default}` never
copies its default into either artifact. Certificate/key paths can appear in Go
as references. Auth data, arbitrary header values and credential-bearing URLs
require manual binding. Parser and canonical-tool diagnostics never echo source
snippets or arbitrary error strings.

Reference reporting recognizes `${NAME...}` and whole-value `$NAME`. Embedded
bare-dollar strings remain manual without being echoed: password-hash components
must not be mistaken for environment variable names.

The generated fragments pass through `Resolve`, `Lint`, `Export` and `GraphDOT`.
Lint rule identifiers remain in the report; error severity blocks acceptance.
Export and graph are discarded rather than embedding configuration values in the
audit. These checks validate the candidate, not missing behavior or live Docker
and certificate state. Regression tests also compile generated Go and compare its
export with the audited configuration.

After completing a skeleton, compile it and run `-validate`, `-lint`, `-export`
and `-graph`. Compare source objects and host coverage with the completed topology,
then perform the migration tracker's side-by-side network checks before cutover.
A successful offline audit alone is not a cutover verdict.
