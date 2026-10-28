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

## Translation coverage

- Static addresses become explicit HTTP or HTTPS listeners. The tool resolves
  router entrypoint membership and default entrypoints before generating listeners.
  A router restricted to only part of the native shared content-listener set is
  blocked. Common entrypoint middleware is prepended to file routes and registered
  as Docker `DefaultMiddleware`; different per-listener chains remain blocked.
- Named ACME resolvers generate `AutoTLS` sources using declared domains or literal
  `Host` matchers. HTTP-01 and Cloudflare DNS-01 are supported. Cloudflare uses a
  required `CF_DNS_API_TOKEN` environment binding. Source ACME JSON storage becomes
  a new sibling `.statute` directory: this starts new native account/certificate
  state and does not import Traefik's account keys. Review issuance and CA limits
  before deploying. Other challenge providers and TLS-ALPN-only requirements remain
  explicit gaps.
- Default certificate file references become `StaticTLS`. A single certificate
  from a source certificate list produces a guarded candidate because its SAN
  mapping and fallback selection cannot be determined without reading material.
  Named TLS version/cipher policies and client authentication become listener
  options when all routers on that listener select the same policy. Distinct
  per-SNI protocol policies cannot be collapsed into one listener-wide policy.
- File-provider load balancers become named pools. Backend URLs, Host forwarding,
  named/default transports, timeouts, idle connection limits, root CA files,
  client certificate files and response flushing are translated. Proxy and probe
  traffic share the selected transport. Credential-bearing URLs, base-path
  rewriting and unknown service fields remain blocking.
- Health checks preserve source intervals, timeouts and one-success/one-failure
  transition thresholds. Explicit `followRedirects: false`, accepted status and
  probe Host are supported. Traefik's default cross-origin redirect following
  differs from native same-origin probes and requires a decision.
- Docker services receive one exact-service `PoolPolicy` containing Host, health
  and transport together. Middleware registrations retain the exact label
  reference. Retained Compose labels are checked with the existing Docker parser
  using a synthetic address; this validates label interpretation without contacting
  a daemon or claiming knowledge of the live network.
- `Host`, `Path`, `PathPrefix`, conjunctions and disjunctions use the shared
  Traefik parser. Native routes represent exact paths, catch-all paths and finite
  Traefik host spellings, including trailing-dot variants. File-provider byte
  prefixes and literal paths ending in `/*` are refused because native segment
  matching would change their coverage. Docker retains its byte-prefix matcher.
- File routes follow source priority. Equal-priority overlaps, overlapping
  file/Docker routes and Docker numeric-priority differences remain explicit
  findings. Catch-all candidates become fallback routes only with a precedence
  check. Unmatched traffic retains the native terminal 404.
- IP allow lists, ordered chains, request/response header operations, BasicAuth
  file or environment bindings and canonical `AddPrefix` prefixes become compiled
  route policy. Header values are bound through environment variables; inline
  values are withheld and require relocation. Auth files must contain bcrypt
  hashes; validation is deferred until the generated program loads them.
- Header/path operations after other middleware remain blocked because Statute
  hoists them before authentication, denial and retry paths. ReplacePath and
  StripPrefix candidates identify missing Traefik metadata headers and matching
  differences. RequestBuffer, Retry and gzip/Brotli candidates identify concrete
  resource, response-buffering and negotiation differences. Incomplete middleware
  candidates are emitted as separate review functions and never registered.
- Redirect middleware and entrypoint redirects produce guarded native candidates.
  Traefik uses method-dependent 301/308 or 302/307 statuses, while native redirect
  listeners always return 301. Non-default target ports and custom redirect
  precedence are separately reported; these differences are not silently accepted.
- Shared incoming timeout/header limits become explicit `Defaults`. Different
  per-entrypoint limits remain blocked. File-provider paths are accepted when the
  exact file or complete directory was explicitly supplied; hot reload is replaced
  by regeneration and compilation.

Unrecognized fields default to `manual`; they are not discarded to make validation
pass. See [Docker compatibility](docker.md#traefik-compatibility-traefiklabels) for
the runtime label subset and its refusal behavior.

## Secrets and validation

Reports omit source values. Environment names such as `env:AUTH_TOKEN` and known
file references are retained without evaluation; `${TOKEN:-inline-default}` never
copies its default into either artifact. Certificate/key paths can appear in Go
as references. Inline auth data and header values require explicit relocation into the generated
bindings. Whole-value `$NAME` and `${NAME}` references are preserved as required
runtime lookups. Credential-bearing backend URLs remain blocked. Parser and canonical-tool diagnostics never echo source
snippets or arbitrary error strings.

Reference reporting recognizes `${NAME...}` and whole-value `$NAME`. Embedded
bare-dollar strings remain manual without being echoed: password-hash components
must not be mistaken for environment variable names.

The generated fragments pass through `Resolve`, `Lint`, `Export` and `GraphDOT`.
Lint rule identifiers remain in the report; error severity blocks acceptance.
Export and graph are discarded rather than embedding configuration values in the
audit. Topology checks compare route order, host/path coverage, action/pool links,
middleware counts and Docker registry/exact-service edges with the source plan.
Checks with `deferred: true` used placeholders for environment/auth bindings or
require runtime certificate material; they prove structural validity only.
Missing bindings fail before startup. The human report also prints deferred state. These checks validate the candidate, not missing behavior or live Docker
and certificate state. Regression tests compile generated Go, compare its export with the audited
configuration, and launch a generated proxy to verify rewritten requests, header
operations, unmatched paths and missing-binding refusal.

After completing a skeleton, compile it and run `-validate`, `-lint`, `-export`
and `-graph`. Compare source objects and host coverage with the completed topology,
then perform the migration tracker's side-by-side network checks before cutover.
A successful offline audit alone is not a cutover verdict.
