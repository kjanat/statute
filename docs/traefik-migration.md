# Offline Traefik migration audit

The migration command inventories Traefik configuration and writes a Go skeleton
plus a JSON audit. It does not start a proxy, contact Docker, acquire certificates,
interpolate environment variables, or perform a cutover. Traefik files do not
become a Statute runtime input format.

Generated listeners select `IngressPolicy` explicitly: untrusted forwarded
headers are replaced with direct-peer values, paths are normalized before routing,
and source alias-header and `sanitizePath` settings are applied. Configured
trusted proxies may supply forwarded identity. Existing native listener defaults
are unchanged. See [listener ingress policy](production.md#listener-ingress-policy).

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
provider-qualified definitions block acceptance. Each identity must have one
unambiguous definition.
TLS stores and named TLS options are inventoried separately, so their definitions
can live in different dynamic files. Each resource retains its source filename
and field paths; duplicate definitions of the same store or option remain blocking.

Output files must be new, distinct paths. They are created with mode `0600` and
never overwrite existing files. Parsing/writing failures remove newly reserved
outputs. Inputs are bounded to 256 files of at most 8 MiB each. Directory symlinks
and multiple YAML documents in one file are rejected.

## Reading the audit

The human report goes to stdout. JSON schema version `1` includes source paths,
provider-qualified identities, every inventoried field's classification,
reference names, source router and distinct host counts, rules with unknown
counts, generated route counts, unmatched objects, and canonical-tool checks.
Compose inventory covers Traefik labels and mechanisms that could alter them.
Unrelated application images, environment values and labels are excluded.

| Classification | Meaning                                                                                                      |
| -------------- | ------------------------------------------------------------------------------------------------------------ |
| `exact`        | A supported source setting retains its existing semantics.                                                   |
| `translated`   | An explicit native representation or retained label implements the classified setting. Read its explanation. |
| `manual`       | A decision, missing input, secret binding, or semantic difference prevents automatic acceptance.             |
| `unsupported`  | The parser or implementation cannot represent the behavior.                                                  |
| `out_of_scope` | The object is intentionally outside this HTTP tool, such as the Traefik dashboard.                           |

An object's classification reflects its most restrictive field. `generated`
means it contributed a fragment or retained Docker setting. Check the field
classifications separately for blocking gaps. TLS paths, for example, can contribute an HTTPS candidate while coverage
remains blocking. `unmatched_objects` lists objects without a contribution.
Disjunctions and host spelling expansion can produce several native routes from
one router. Docker counts describe parsed labels; live discovery is outside the audit.

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
  Generated ingress decodes unreserved path escapes and normalizes escape casing.
  Source `http.sanitizePath` additionally cleans dot segments and repeated slashes;
  it defaults to enabled and can be explicitly disabled. Query text is preserved.
  Alias headers can be kept, deleted or rejected before routing.

- Named ACME resolvers generate `AutoTLS` sources using declared domains or literal
  `Host` matchers. HTTP-01 and Cloudflare DNS-01 are supported. Cloudflare uses a
  required `CF_DNS_API_TOKEN` environment binding. Source ACME JSON storage becomes
  a new sibling `.statute` directory: this starts new native account/certificate
  state and does not import Traefik's account keys. Review issuance and CA limits
  before deploying. Other challenge providers and TLS-ALPN-only requirements remain
  explicit gaps. DNS resolvers and propagation delay generate `Propagation`
  candidates. Traefik's authoritative-server checks differ from native propagation
  checking; the report requests that specific choice. Native pinned ACME sources issue ECDSA P-256 certificates, requiring
  an explicit source `keyType: EC256`. An omitted key type defaults to RSA4096 in
  Traefik and blocks migration until certificate algorithm compatibility is reviewed.
- Default certificate file references become `StaticTLS`. A single certificate
  from a source certificate list produces a guarded candidate because its SAN
  mapping and fallback selection cannot be determined without reading material.
  Named TLS version/cipher policies and client authentication become listener
  options when all routers on that listener select the same policy. Distinct
  per-SNI protocol policies cannot be collapsed into one listener-wide policy.
  Named options on a rule without literal Host coverage are blocked: Traefik uses
  its default TLS policy for hostless rules, while a native listener-wide option
  would change version or client-authentication requirements.
  A router inherits the complete entrypoint TLS object only when its own TLS object
  is absent. Explicit router TLS, including an empty object, prevents inheritance
  of entrypoint options, certificate resolvers and certificate domains.
- File-provider load balancers become named pools. Backend URLs, Host forwarding,
  named/default transports, timeouts, idle connection limits, root CA files,
  client certificate files and response flushing are translated. Proxy and probe
  traffic share the selected transport. Credential-bearing URLs, base-path
  rewriting and unknown service fields remain blocking.
  Explicit `default@internal` transport references use the audited static transport
  defaults for both file services and Docker service policies.
- Health checks preserve source intervals, timeouts and one-success/one-failure
  transition thresholds. Explicit `followRedirects: false`, accepted status and
  probe Host are supported. Traefik's default cross-origin redirect following
  differs from native same-origin probes and requires a decision.
  Probe paths must preserve their URL meaning as native literal paths. Query
  strings, fragments, percent escapes, authority changes and dot-segment resolution
  produce blocking findings because Traefik resolves these as relative URLs.
  Health-check candidates always report the all-unhealthy behavior difference:
  Traefik stops forwarding and returns 503, while Statute's degraded mode continues
  trying primary backends. This requires a migration decision for file and Docker
  services, including single-backend services. Probe settings remain in the guarded
  candidate for review. Passive windows and thresholds generate pool policy.
  Review success resets, exclusion duration and degraded forwarding before enabling it.
- Docker services receive one exact-service `PoolPolicy` containing Host, health
  and transport together. Middleware registrations retain the exact label
  reference. Retained Compose labels are checked with the existing Docker parser
  using a synthetic address; this validates label interpretation without contacting
  a daemon or claiming knowledge of the live network. Routers without a service
  reference select the container's sole declared service, or an implicit service
  from one explicit Compose TCP target port. Multiple services or missing port
  evidence require a decision. Cross-provider references produce a guarded compiled
  route helper with the provider-qualified target; they never select another pool.
- `Host`, `Path`, `PathPrefix`, conjunctions and disjunctions use the shared
  Traefik parser. Native routes represent exact paths, catch-all paths and finite
  Traefik host spellings, including trailing-dot variants. File-provider byte
  prefixes and literal paths ending in `/*` are refused because native segment
  matching would change their coverage. Docker retains its byte-prefix matcher.
- File routes follow source priority. Equal-priority overlaps, overlapping
  file/Docker routes and Docker numeric-priority differences remain explicit
  findings, consolidated into one ordering decision naming the conflicting
  routes and priorities. Catch-all candidates become fallback routes only with a
  precedence check. Unmatched traffic retains the native terminal 404.
- Fixed security headers generate response operations: nosniff, frame denial,
  HSTS directives and standard referrer policies. HSTS is HTTPS-only unless the
  source forces emission. Custom headers override security headers within one
  middleware; arbitrary values use environment bindings. Conditional host, TLS
  and CORS behavior requires a separate policy decision.
- IP allow lists, ordered chains, request/response header operations, BasicAuth
  file or environment bindings and canonical `AddPrefix` prefixes become compiled
  route policy. Header values are bound through environment variables; inline
  values are withheld and require relocation. Rate limits generate route-owned
  candidates, with explicit findings for independent burst capacity, bounded
  client-key admission, delayed source reservations and unsupported key selection. Auth files must contain bcrypt
  hashes; validation is deferred until the generated program loads them.
- Header/path operations after other middleware remain blocked because Statute
  hoists them before authentication, denial and retry paths. Conflicting operations
  on the same response header also block: Traefik applies nested response modifiers
  from the inner middleware outward, while Statute applies operations in declaration
  order. This check includes nested chains and entrypoint defaults for file and
  Docker routes, and compares header names case-insensitively. ReplacePath and
  StripPrefix candidates identify missing Traefik metadata headers and matching
  differences. RequestBuffer, Retry and gzip/Brotli candidates identify concrete
  resource, response-buffering and negotiation differences. Incomplete middleware
  candidates are emitted as separate review functions and never registered.
- Redirect schemes and supported full-URI regex captures generate native redirects
  with Traefik's GET/non-GET status split, including HEAD. Queries and captures
  remain part of `{request_uri}`. Standard ports are omitted from scheme targets;
  explicit ports are preserved. Literal-authority regexes produce guarded
  candidates because raw Host case/port matching and non-match fallthrough differ
  from native host routing. An equal replacement URL also needs a decision because
  Traefik continues to the backend. Entrypoint redirect precedence is checked
  separately from its target and method statuses.

- Shared incoming timeout/header limits become explicit `Defaults`. Different
  per-entrypoint limits remain blocked. File-provider paths are accepted when the
  exact file or complete directory was explicitly supplied; hot reload is replaced
  by regeneration and compilation.

Blocked middleware is reported at its definition with one dependency finding per
affected route. Unsupported matchers identify the matcher kind. Dashboard, file
watching, plugins and TCP/UDP are reported as out of scope.

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
Export and graph output is discarded to keep configuration values out of the
audit. Topology checks compare route order, host/path coverage, action/pool links,
middleware counts and Docker registry/exact-service edges with the source plan.
Checks with `deferred: true` used placeholders for environment/auth bindings or
require runtime certificate material; they prove structural validity only.
Missing bindings fail before startup. The human report also prints deferred state. These checks validate the candidate.
Missing behavior and live Docker/certificate state require deployment validation. Regression tests compile generated Go, compare its export with the audited
configuration, and launch a generated proxy to verify rewritten requests, header
operations, unmatched paths and missing-binding refusal.

After completing a skeleton, compile it and run `-validate`, `-lint`, `-export`
and `-graph`. Compare source objects and host coverage with the completed topology,
then perform the migration tracker's side-by-side network checks before cutover.
A successful offline audit alone is not a cutover verdict.
