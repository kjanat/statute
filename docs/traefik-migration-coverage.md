# Traefik migration validation

Use [the migration guide](traefik-migration.md) for supported inputs, bindings and
remaining gaps. An audit's route count describes generated configuration; it does
not establish that private certificates, credentials or Docker backends work.

Validation covers three different boundaries:

| Boundary               | What it establishes                                                                                                                           |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| Audit and generated Go | Every inventoried field has a disposition; Resolve, Lint, Export and Graph see consistent configuration.                                      |
| Native runtime         | Network responses, middleware order, Retry, listener isolation, TLS handshakes, cache protection, buffering and cleanup behave as configured. |
| Traefik comparison     | Differential fixtures compare generated behavior with the Traefik middleware version declared in `traefikoracle/go.mod`.                      |

Generated-program tests compile and execute the emitted entrypoint. Unsupported
behavior refuses startup. Missing environment bindings fail before listeners
open, while supplied bindings allow structural validation. These checks do not
read the deployment's private configuration or contact its Docker daemon.

Runtime regressions cover credentials introduced inside middleware groups,
forwarded-scheme redirects, escaped paths under Retry, response-encoding
negotiation and streaming, SNI policy replacement, and DNS resolver fallback.
Lifecycle checks exercise failed-start cleanup and subsequent serving, including
buffered file logging. Docker refusal tests check that unavailable authentication
cannot fall through to a broader route or application fallback.

Run `make test`, `make test-race`, `make lint`, `make lint-lifecycle` and
`make test-e2e-regression`. CI reports coverage separately. Internal export parity
and coverage percentages alone do not establish Traefik compatibility or cutover
readiness.
