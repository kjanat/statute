package main

import (
	"os"
	"testing"
	"time"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/resolved"
)

func resolvedExample(t *testing.T) *resolved.Config {
	t.Helper()
	cfg, err := statute.Resolve(exampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Docker == nil {
		t.Fatal("example has no Docker discovery")
	}
	return cfg
}

func TestExampleDockerDiscovery(t *testing.T) {
	t.Parallel()
	cfg := resolvedExample(t)
	if cfg.Docker.Storage != "/var/lib/statute/docker" || cfg.Docker.Endpoint != "unix:///var/run/docker.sock" {
		t.Fatalf("Docker storage/endpoint do not match Compose mounts: %+v", cfg.Docker)
	}
	if cfg.Docker.ExposedByDefault || cfg.Docker.Refresh != 0 {
		t.Fatal("example must discover labeled containers using events without periodic refresh")
	}
	if len(cfg.Routes) != 0 || len(cfg.Upstreams) != 0 || cfg.Fallback != nil {
		t.Fatal("example must derive routes from Docker labels without a static fallback")
	}
}

// TestExampleWorkloadPolicy checks the contract shared with compose.yml: only
// its labeled service has authority, and HTTP readiness gates the cold route.
func TestExampleWorkloadPolicy(t *testing.T) {
	t.Parallel()
	cfg := resolvedExample(t)
	policy, ok := cfg.Docker.Workloads["ondemand-demo"]
	if !ok || len(cfg.Docker.Workloads) != 1 {
		t.Fatalf("lifecycle authority must be restricted to ondemand-demo: %+v", cfg.Docker.Workloads)
	}
	want := resolved.Workload{
		IdleAfter: 10 * time.Second, StartTimeout: 10 * time.Second, ReadyTimeout: 20 * time.Second,
		BackoffBase: 5 * time.Second, BackoffCap: 5 * time.Minute,
		Readiness: resolved.WorkloadReadiness{Mode: resolved.ReadinessHTTP, Path: "/health"},
	}
	if policy != want {
		t.Errorf("workload policy: got %+v, want %+v", policy, want)
	}
}

func TestExampleListenerAndDiagnostics(t *testing.T) {
	t.Parallel()
	cfg := resolvedExample(t)
	if len(cfg.Listeners) != 1 || cfg.Listeners[0].Addr != ":8080" || cfg.Listeners[0].Scheme != "http" {
		t.Fatalf("content listener does not match Compose port: %+v", cfg.Listeners)
	}
	metrics := resolved.Metrics{Enabled: true, Kind: "prometheus", Addr: ":9090", Path: "/metrics"}
	if cfg.Observability.Metrics != metrics {
		t.Errorf("metrics listener: got %+v, want %+v", cfg.Observability.Metrics, metrics)
	}
	health := resolved.Health{Enabled: true, Addr: ":8081", Path: "/healthz"}
	if cfg.Observability.Health != health {
		t.Errorf("health listener: got %+v, want %+v", cfg.Observability.Health, health)
	}
}

func TestExampleLoggingAndShutdown(t *testing.T) {
	t.Parallel()
	cfg := resolvedExample(t)
	log := cfg.Observability.AccessLog
	if !log.Enabled || log.Format != "json" || log.Writer != os.Stdout {
		t.Errorf("access log must use JSON on stdout: %+v", log)
	}
	if cfg.Shutdown != (resolved.Shutdown{GracePeriod: 10 * time.Second, DrainListeners: true}) {
		t.Errorf("shutdown must drain requests within the example's grace period: %+v", cfg.Shutdown)
	}
}
