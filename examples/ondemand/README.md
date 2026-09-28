# On-demand Docker workload

This runnable example starts a stopped container on the first request, waits for
HTTP readiness, returns the original POST bytes in a JSON/base64 envelope, and
stops the container ten seconds after the last request finishes. Run one copy at
a time on a Docker host.

Prerequisites: Docker Engine with Compose v2, `curl`, `jq`, and free loopback ports
8080, 8081, and 9090. The proxy runs in Docker so it can reach the origin's bridge
address, including when the daemon runs inside Docker Desktop.

From the repository root:

```sh
docker compose -f examples/ondemand/compose.yml build proxy
docker compose -f examples/ondemand/compose.yml create origin
docker compose -f examples/ondemand/compose.yml up -d --no-deps proxy
curl --fail --retry 30 --retry-all-errors --retry-delay 1 \
  http://127.0.0.1:8081/healthz/ready
```

Do not run `compose up` for the whole stack: that would start the origin before
the first request. Verify that it is still stopped:

```sh
docker inspect --format '{{.State.Status}}' \
  "$(docker compose -f examples/ondemand/compose.yml ps -aq origin)"
# created
curl --fail --max-time 30 -H 'Host: ondemand.local' \
  --data-binary 'first cold request' http://127.0.0.1:8080/upload | jq -r '.body | @base64d'
# first cold request
curl --fail http://127.0.0.1:9090/debug/workloads
```

The response's `body` field contains the submitted bytes encoded as base64. This
keeps arbitrary payloads out of raw HTML; the commands decode the demonstration's
text with `jq`. The decoded body must match exactly. Diagnostics report a ready
owner and one activation. Observe idle shutdown without sending traffic to the
workload (health/metrics requests do not hold it active):

```sh
sleep 12
docker inspect --format '{{.State.Status}}' \
  "$(docker compose -f examples/ondemand/compose.yml ps -aq origin)"
# exited (if Docker is slow, inspect again until the idle stop settles)
curl --fail http://127.0.0.1:9090/metrics
curl --fail --max-time 30 -H 'Host: ondemand.local' \
  --data-binary 'second cold request' http://127.0.0.1:8080/upload | jq -r '.body | @base64d'
# second cold request
```

The second request must wake the same container again. Activation and idle-stop
counters appear under `statute_docker_workload_*` with service `ondemand-demo`.

## Authority, storage, and cleanup

The Docker socket allows host-level control; only run this example on a daemon
you intend it to control. Lifecycle authority requires the compiled `Workload`
policy. The origin uses `restart: "no"`; do not run another
lifecycle controller or a second proxy for this container.

The named `mutations` volume stores outstanding mutation ownership. Preserve it
across ordinary proxy restarts. Metrics and diagnostics are unauthenticated;
Compose publishes them only on loopback. Do not publish them publicly.

When finished, this removes **the example's containers and persistent mutation
volume**. Use it only for full demo teardown; preserve the volume during ordinary
proxy restarts:

```sh
docker compose -f examples/ondemand/compose.yml down -v
```

See [Docker workloads](../../docs/docker.md#on-demand-workloads-workload) for the
production contract and [diagnostics](../../docs/observability.md#workload-snapshots)
for counter semantics and failure reasons.
