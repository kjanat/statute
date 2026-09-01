package statute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestMutationRegistryPersistsOutstandingRecords(t *testing.T) {
	dir := t.TempDir()
	registry := mustOpenMutationRegistry(t, dir, "unix:///run/docker.sock")
	for _, record := range []mutationRecord{
		{ContainerID: "container-b", ContainerName: "b", Service: "api", Kind: mutationRecordCleanupStop, State: mutationRecordPrepared},
		{ContainerID: "container-a", ContainerName: "a", Service: "web", Kind: mutationRecordIdleStop, State: mutationRecordPrepared},
	} {
		mustMutationRegistryOperation(t, "put "+record.ContainerID, registry.put(record))
	}
	mustMutationRegistryOperation(t, "mark uncertain", registry.markUncertain("container-a"))

	reopened := mustOpenMutationRegistry(t, dir, "unix:///run/docker.sock")
	records := reopened.list()
	if len(records) != 2 {
		t.Fatalf("reopened records = %d, want 2", len(records))
	}
	if records[0].ContainerID != "container-a" || records[0].State != mutationRecordUncertain {
		t.Fatalf("first reopened record = %+v", records[0])
	}
	if records[1].ContainerID != "container-b" || records[1].Kind != mutationRecordCleanupStop {
		t.Fatalf("second reopened record = %+v", records[1])
	}
	mustMutationRegistryOperation(t, "delete", reopened.delete("container-a"))
	settled := mustOpenMutationRegistry(t, dir, "unix:///run/docker.sock")
	if settled.contains("container-a") || !settled.contains("container-b") {
		t.Fatalf("settled records = %+v", settled.list())
	}
}

func mustOpenMutationRegistry(t *testing.T, root, endpoint string) *mutationRegistry {
	t.Helper()
	registry, err := openMutationRegistry(root, endpoint)
	if err != nil {
		t.Fatalf("open mutation registry: %v", err)
	}
	return registry
}

func mustMutationRegistryOperation(t *testing.T, operation string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

func TestMutationRegistryRejectsWrongEndpoint(t *testing.T) {
	dir := t.TempDir()
	registry, err := openMutationRegistry(dir, "unix:///run/docker-a.sock")
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	if err := registry.put(mutationRecord{ContainerID: "container-a", Service: "web", Kind: mutationRecordIdleStop, State: mutationRecordPrepared}); err != nil {
		t.Fatalf("put record: %v", err)
	}
	_, err = openMutationRegistry(dir, "unix:///run/docker-b.sock")
	if err == nil || !strings.Contains(err.Error(), "endpoint") {
		t.Fatalf("wrong-endpoint error = %v", err)
	}
}

func TestMutationRegistryRequiresExistingStorageDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing", "docker")
	if _, err := openMutationRegistry(root, "unix:///run/docker.sock"); err == nil {
		t.Fatal("missing storage directory was created implicitly")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("missing storage path exists or has unexpected error: %v", err)
	}
}

func TestMutationRegistryRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, mutationRegistryFile), []byte("{"), 0o600); err != nil {
		t.Fatalf("write corrupt registry: %v", err)
	}
	if _, err := openMutationRegistry(dir, "unix:///run/docker.sock"); err == nil {
		t.Fatal("corrupt mutation registry was accepted")
	}
}

func TestMutationRegistryCorruptionFailsBeforeRoutePublication(t *testing.T) {
	cfg := &resolved.Docker{Workloads: map[string]resolved.Workload{"wl": testWorkloadPolicy()}}
	p, srv, _ := newFakeProviderDaemon(t, cfg, nil)
	if err := os.WriteFile(filepath.Join(cfg.Storage, mutationRegistryFile), []byte("{"), 0o600); err != nil {
		t.Fatalf("write corrupt registry: %v", err)
	}
	if _, err := p.start(); err == nil {
		t.Fatal("provider accepted corrupt mutation registry")
	}
	if srv.dynamic.Load() != nil {
		t.Fatal("provider published routes before validating mutation registry")
	}
}
