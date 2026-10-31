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

The generated `main.go` has one function per section: `generatedListeners`,
`generatedUpstreams`, `generatedRoutes` and `generatedDocker`. A comment naming the source object, such as
`// routers:api@file`, precedes each listener, pool and route. Long method chains
and structs with several fields put each call or field on its own line, and
zero-valued fields are omitted. Identical inputs produce byte-identical source.

Each middleware a route uses becomes one function named after its source, such
as `securityHeaders()` for `security-headers@file`. It returns one
`statute.Middleware`, or a `[]statute.Middleware` for a Traefik `chain` that
yields several. Routes, entrypoint defaults and the Docker middleware registry
call that function; consecutive single middleware share one argument list, as
in `With(securityHeaders(), rateLimit())`. Each `serversTransport`, the default
transport, and each service that overrides its flush interval becomes a
function such as `backendTransport()`, shared by the upstream pools and Docker
pool policies that use it. Every source keeps its own function, even when two
sources have the same settings, so each one remains separately editable. When
at least two emitted functions build the same kind of rate limit, they call one
shared constructor, such as `makeRateLimit(rateLimitSettings{Rate: "20/s", ...})`,
with their own values in named fields. A name that collides with another function, an import
or a Go identifier receives a numeric suffix. Each call returns new values, so
routes using the same function keep separate rate limits, caches and buffers.

A router whose rule names several hosts becomes one route listing all of them in
`Hosts(...)` when the routes for those hosts differ only by host. Statute expands
that list into one route per host, each with its own middleware state. When at
least two file pools have one backend and only a transport, they call
`makePool(address, transport)`. Routes and Docker bindings for `noop@internal`
share one `migrationNoop()` handler.

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
Generated route counts describe candidates, including host spelling expansion
and disjunctions. They do not count accepted source routers or prove readiness
to serve. A generated candidate can still have blocking findings. Docker counts describe parsed labels; live discovery is outside the audit.

| Exit status | Meaning                                                       |
| ----------- | ------------------------------------------------------------- |
| `0`         | Help displayed, or no blocking findings or validation errors. |
| `1`         | Artifacts were written, but migration gaps remain.            |
| `2`         | Invalid invocation/input or artifact I/O failure.             |

Unsupported behavior guards generated startup. Fix the source inputs or supply
missing inputs and regenerate; keep that guard in place. Credential relocation
uses required environment bindings. The generated program validates those
bindings before opening listeners, so a binding-only audit can produce a usable
program while still reporting `blocking: true` until its inputs are supplied.

## Translation coverage

- Listeners have names matching source entrypoints. Router membership and
  entrypoint middleware use those names, so private routes and different
  listener chains stay scoped. Shared timeout/header limits become explicit
  `Defaults`; differing per-entrypoint limits remain blocking.
- Ingress replaces untrusted forwarded headers with direct-peer values and
  accepts configured trusted proxies. Escape normalization precedes routing.
  Source `sanitizePath` additionally cleans dot segments and repeated slashes;
  query text is preserved. Alias headers can be kept, deleted or rejected.
- `Host`, `Path`, `PathPrefix` and `ClientIP`, with conjunctions and disjunctions,
  use the shared rule parser. Exact paths and byte prefixes have native matchers.
  `ClientIP` checks the connection peer. Source priorities apply across file and
  Docker routes; equal priorities have deterministic declaration order. A
  rejected security policy still refuses its traffic before broader routes or
  application fallback can serve it.
- Middleware groups execute in source order. Header and path operations run at
  each group's position, including on Retry re-entry. `ReplacePath` and
  `ReplacePathRegex` preserve escaped captures and `X-Replaced-Path`;
  `StripPrefix` preserves byte-prefix matching and `X-Forwarded-Prefix`.
- Redirect middleware expands regex captures over the full request URL. A
  non-match or unchanged target continues through the chain. GET uses 301/302
  and other methods use 308/307 unless the source selects a fixed status.
  Entrypoint redirects use listener-scoped routes at their source priority.
  Trusted forwarded schemes suppress redirects when the target already matches.
- Security headers, custom header operations, IP allow lists and BasicAuth
  become route middleware. Standard public policy values can appear in Go;
  arbitrary header values and inline bcrypt hashes use required environment
  bindings. Auth files are loaded by the generated program, and must contain
  bcrypt hashes. The offline audit does not load credentials.
- Rate limits preserve independent burst capacity, delayed reservations and
  source key selection: peer address, forwarded-chain depth/exclusions, Host or
  a request header. State belongs to each route. Idle expiry and oldest-bucket
  replacement bound the native store.
- Body buffering supports request/response limits, memory spill thresholds and
  the source retry expression. Spill files belong to the request. An unlimited
  source maximum becomes the native 8 MiB bound, and each route admits 128
  concurrent buffered requests; the report notes both. All routes share the
  server's 1 GiB `BufferBodiesBudget`. Response
  encoding supports gzip, Brotli and zstd, media-type filters and a minimum body
  size. A standalone Traefik Retry policy remains blocking where its transport
  failure behavior differs from native buffered Retry.
- File load balancers become named pools. Host forwarding, transport timeouts,
  idle limits, root CA files, client certificates and response flushing are
  translated. Proxy and probe traffic share the pool transport. Credential URLs,
  upstream base-path rewriting and unknown service fields remain blocking.
- Health checks preserve URL-reference resolution, method, accepted status,
  headers and redirect policy. Probe credentials use required environment
  bindings. `RejectUnhealthy` produces 503 when no backend is healthy; passive
  health can reset on success and hold exclusion for the source failure window.
- Docker policies use exact service names. Routers with no service reference use
  the container's sole declared service or one explicit Compose TCP target port.
  Ambiguous services and missing ports remain blocking. Docker routers can bind
  a compiled file-provider pool; compiled routes can select a Docker service.
  A service with no Docker router is discovered when a compiled route names it.
  Label validation uses a synthetic address and makes no claim about live
  container networking.
- TLS version/cipher policies and client authentication are selected by SNI.
  TLS 1.2 cipher restrictions must support the selected pinned ACME source's key
  type; incompatible policies fail `Resolve`.
  Selected hostname policies replace the listener defaults. HTTP Host reuse
  across different TLS policies returns 421. Hostless rules use the default
  source policy. Router TLS inherits the complete entrypoint object only when
  the router has no TLS object; an explicit empty object prevents inheritance.
  Strict TLS conflicts between Docker routers block migration: discovery cannot
  enforce the source's router exclusions. File-router exclusions are generated.
  TLS options from providers other than `file` block migration.
- ACME supports HTTP-01 and Cloudflare DNS-01. Source key types include EC256,
  EC384 and RSA2048/3072/4096/8192; omission selects Traefik's RSA4096 default.
  DNS propagation can verify authoritative servers and recursive resolvers,
  apply a delay, or explicitly disable checks. Discovery queries, the IPv4
  and IPv6 lookups for each authoritative nameserver, and its addresses run
  concurrently; one matching authoritative answer verifies that nameserver.
  Cloudflare requires `CF_DNS_API_TOKEN`.
- ACME storage uses a new sibling `.statute` directory and starts fresh account
  and certificate state. Traefik account keys and certificates are not imported.
  Review issuance and CA limits before deploying. Other DNS providers and
  TLS-ALPN-only requirements remain explicit gaps.
- Default certificate references become `StaticTLS`. Certificate lists whose
  hostname selection requires reading certificate material remain guarded;
  supply explicit native host mappings. The audit never reads private keys.
- Access logs support common/JSON output, files, a bounded queue and status
  filters. Ping routes preserve listener membership and shutdown status.
  Internal Traefik diagnostic levels, update checks and usage reporting are
  reported as out of scope.

For provider paths inside a container, supply `--path-map SOURCE=LOCAL`. An
explicit mapping takes precedence over Compose bind mounts, and the longest
matching source prefix wins. Inferred mounts keep their Compose service scope:
a nested mount shadows its parent within that service. Mount inference uses only services that mount a
supplied static file. Provider inputs must still be supplied explicitly with
`--dynamic`; mappings do not authorize reading arbitrary container paths.

Unsupported middleware is reported at its definition with one dependency finding
per affected route. Unsupported matchers identify the matcher kind. Dashboard
routes, file watching and TCP/UDP are outside the HTTP migration. Plugins cannot
become empty chains: a route referencing an unavailable plugin stays blocked.
Unrecognized fields remain `manual`.

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
