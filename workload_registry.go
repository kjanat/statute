package statute

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	mutationRegistryVersion = 1
	mutationRegistryFile    = "outstanding-mutations"
)

type mutationRecordKind string

const (
	mutationRecordIdleStop    mutationRecordKind = "idle-stop"
	mutationRecordCleanupStop mutationRecordKind = "cleanup-stop"
)

type mutationRecordState string

const (
	mutationRecordPrepared  mutationRecordState = "prepared"
	mutationRecordUncertain mutationRecordState = "uncertain"
)

type mutationRecord struct {
	ContainerID   string              `json:"container_id"`
	ContainerName string              `json:"container_name,omitempty"`
	Service       string              `json:"service"`
	Kind          mutationRecordKind  `json:"kind"`
	State         mutationRecordState `json:"state"`
}

type mutationRegistrySnapshot struct {
	Version  int              `json:"version"`
	Endpoint string           `json:"endpoint"`
	Records  []mutationRecord `json:"records"`
}

type mutationRegistry struct {
	mu       sync.Mutex
	root     string
	endpoint string
	records  map[string]mutationRecord
}

func openMutationRegistry(root, endpoint string) (*mutationRegistry, error) {
	if root == "" {
		return nil, errors.New("storage path is empty")
	}
	info, err := os.Stat(root) //nolint:gosec // the operator explicitly configures this durable state root
	if err != nil {
		return nil, fmt.Errorf("open storage directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("storage path is not a directory")
	}
	r := &mutationRegistry{root: root, endpoint: endpoint, records: map[string]mutationRecord{}}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *mutationRegistry) load() error {
	f, err := os.Open(filepath.Join(r.root, mutationRegistryFile))
	if errors.Is(err, os.ErrNotExist) {
		return r.persist(r.records)
	}
	if err != nil {
		return fmt.Errorf("open mutation registry: %w", err)
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var snapshot mutationRegistrySnapshot
	if err := dec.Decode(&snapshot); err != nil {
		return fmt.Errorf("decode mutation registry: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return fmt.Errorf("decode mutation registry: %w", err)
	}
	if snapshot.Version != mutationRegistryVersion {
		return fmt.Errorf("mutation registry version %d is unsupported", snapshot.Version)
	}
	if snapshot.Endpoint != r.endpoint {
		return fmt.Errorf("mutation registry endpoint %q does not match %q", snapshot.Endpoint, r.endpoint)
	}
	for _, record := range snapshot.Records {
		if err := validateMutationRecord(record); err != nil {
			return err
		}
		if _, duplicate := r.records[record.ContainerID]; duplicate {
			return fmt.Errorf("mutation registry has duplicate container ID %q", record.ContainerID)
		}
		r.records[record.ContainerID] = record
	}
	return nil
}

func requireJSONEOF(dec *json.Decoder) error {
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateMutationRecord(record mutationRecord) error {
	if record.ContainerID == "" {
		return errors.New("mutation registry record has empty container ID")
	}
	if record.Service == "" {
		return fmt.Errorf("mutation registry record %q has empty service", record.ContainerID)
	}
	switch record.Kind {
	case mutationRecordIdleStop, mutationRecordCleanupStop:
	default:
		return fmt.Errorf("mutation registry record %q has invalid kind %q", record.ContainerID, record.Kind)
	}
	switch record.State {
	case mutationRecordPrepared, mutationRecordUncertain:
	default:
		return fmt.Errorf("mutation registry record %q has invalid state %q", record.ContainerID, record.State)
	}
	return nil
}

func (r *mutationRegistry) list() []mutationRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return sortedMutationRecords(r.records)
}

func (r *mutationRegistry) contains(containerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.records[containerID]
	return ok
}

func (r *mutationRegistry) put(record mutationRecord) error {
	if err := validateMutationRecord(record); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.records[record.ContainerID]; ok {
		if existing.Service != record.Service || existing.Kind != record.Kind {
			return fmt.Errorf("container %q already owns %s for service %q", record.ContainerID, existing.Kind, existing.Service)
		}
		return nil
	}
	next := cloneMutationRecords(r.records)
	next[record.ContainerID] = record
	if err := r.persist(next); err != nil {
		return err
	}
	r.records = next
	return nil
}

func (r *mutationRegistry) markUncertain(containerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[containerID]
	if !ok || record.State == mutationRecordUncertain {
		return nil
	}
	record.State = mutationRecordUncertain
	next := cloneMutationRecords(r.records)
	next[containerID] = record
	if err := r.persist(next); err != nil {
		return err
	}
	r.records = next
	return nil
}

func (r *mutationRegistry) delete(containerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.records[containerID]; !ok {
		return nil
	}
	next := cloneMutationRecords(r.records)
	delete(next, containerID)
	if err := r.persist(next); err != nil {
		return err
	}
	r.records = next
	return nil
}

func (r *mutationRegistry) persist(records map[string]mutationRecord) error {
	tmp, err := os.CreateTemp(r.root, ".outstanding-mutations-")
	if err != nil {
		return fmt.Errorf("create mutation registry temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() //nolint:gosec // CreateTemp returned this path under the configured root
	fail := func(op string, err error) error {
		_ = tmp.Close()
		return fmt.Errorf("%s mutation registry: %w", op, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail("chmod", err)
	}
	snapshot := mutationRegistrySnapshot{
		Version:  mutationRegistryVersion,
		Endpoint: r.endpoint,
		Records:  sortedMutationRecords(records),
	}
	if err := json.NewEncoder(tmp).Encode(snapshot); err != nil {
		return fail("encode", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close mutation registry: %w", err)
	}
	path := filepath.Join(r.root, mutationRegistryFile)
	if err := os.Rename(tmpName, path); err != nil { //nolint:gosec // fixed filename under the operator-configured storage root
		return fmt.Errorf("replace mutation registry: %w", err)
	}
	dir, err := os.Open(r.root) //nolint:gosec // the operator explicitly configures this durable state root
	if err != nil {
		return fmt.Errorf("open mutation registry directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync mutation registry directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close mutation registry directory: %w", err)
	}
	return nil
}

func cloneMutationRecords(records map[string]mutationRecord) map[string]mutationRecord {
	out := make(map[string]mutationRecord, len(records))
	maps.Copy(out, records)
	return out
}

func sortedMutationRecords(records map[string]mutationRecord) []mutationRecord {
	out := make([]mutationRecord, 0, len(records))
	for _, record := range records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ContainerID < out[j].ContainerID })
	return out
}

func mutationRecordKindForStop(kind workloadStopKind) mutationRecordKind {
	switch kind {
	case workloadIdleStop:
		return mutationRecordIdleStop
	case workloadCleanupStop:
		return mutationRecordCleanupStop
	default:
		return ""
	}
}

func workloadStopKindForRecord(kind mutationRecordKind) workloadStopKind {
	switch kind {
	case mutationRecordCleanupStop:
		return workloadCleanupStop
	default:
		return workloadIdleStop
	}
}
