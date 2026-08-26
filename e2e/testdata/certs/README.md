# e2e test PKI

Throwaway, test-only certificate material for the e2e lane's private
Docker network. Nothing here protects anything: the CA key is checked in
on purpose so scenarios are reproducible without a generation step.

- `ca.crt` / `ca.key` — the lane's root, trusted by clients via the plan
  `roots_file` and by Statute's upstream-TLS scenarios.
- `statute.crt` / `statute.key` — leaf served by Statute nodes
  (SANs: `statute-1`, `statute-2`, `localhost`, `127.0.0.1`).
- `origin.crt` / `origin.key` — leaf served by TLS origins
  (SANs: `origin-1`, `origin-2`, `localhost`, `127.0.0.1`).

Regenerate (10-year validity) with the openssl commands in the git
history of this directory if they ever expire.
