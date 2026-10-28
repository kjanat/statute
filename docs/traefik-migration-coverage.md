# Migration acceptance contract

Issue #241 is an offline audit and generator, with field-level accountability.
An absent implementation is not a reason to label a supported field `manual`.
Known fields generate native configuration or a guarded candidate; a blocking
finding must identify the missing input or concrete semantic difference.

The owner is the offline migration package. Public configuration, canonical
resolution and runtime ownership remain unchanged. Middleware stays on routes,
transport and health on pools, and TLS and ingress policy on listeners. Source
provider identity and middleware declaration order survive normalization.

One audit invocation owns input documents, the normalized inventory and artifact
files. It does not acquire runtime resources. Referenced secret material is not
read by the audit. Explicit generated environment/file bindings fail closed before
startup. Validation that depends on these inputs is reported as deferred. Blocking source findings guard the entire generated program.

| Acceptance area      | Required evidence                                                                                                                              |
| -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Input accounting     | YAML/TOML and Compose map/list inputs, supplied file-provider coverage, duplicates and unknown fields                                          |
| Normalization        | Qualified references, ordered middleware, rules, source priorities, listeners, TLS source assignments and service dependencies                 |
| Listeners and TLS    | HTTP-to-HTTPS entrypoint redirect, named ACME resolver with declared/inferred domains, TLS options and explicit file references                |
| Pools                | File-provider health and transport; Docker Host plus health plus transport in one exact-service policy                                         |
| Middleware           | Header operations and bindings, IP policy, chains, authentication file bindings, rewrites and precise Retry/buffering differences              |
| Routing              | Traefik host spellings, exact/catch-all paths, priority and fallback ordering, listener reachability; refusal of unrepresentable byte prefixes |
| Security             | No inline credentials in either artifact; missing/invalid bindings and missing named policies cannot serve                                     |
| Canonical validation | Generated Go compiles; resolved routes, actions, pools, middleware and listener links agree with the source translation plan                   |
| Offline operation    | No Docker, ACME, DNS, environment lookup or secret-file access while auditing                                                                  |
| Completion           | A mixed deployment generates a reviewable candidate with documented semantic gaps; every gap guards startup                                    |

All interaction-matrix rows apply to the generated configuration. Retry and
hoisting are checked at middleware assembly; shared pools and Docker registration
retain route isolation; TLS coverage and selected policy are checked before
listener generation. Process lifecycle is exercised by generated-program startup
refusal and successful serving tests. Observability differences produce explicit
findings and require a migration decision.

Forwarded-header handling currently blocks every generated listener, including
inputs that rely on Traefik defaults. Tests compare the complete expected gap set
and reject additional translation failures. Health candidates
also report the all-unhealthy exclusion difference. Serving tests first prove
that the emitted program refuses startup, then use an explicit test-only entrypoint
to exercise candidate routing and expose the untrusted `X-Real-IP` behavior.

Architectural decisions required: none. Non-equivalent runtime capabilities remain
explicit findings as allowed by #241; this change does not add a second runtime
configuration loader or silently extend native matcher semantics.
