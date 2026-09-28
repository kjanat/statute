package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"statute.kjanat.dev/internal/cloudflare"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureClient(t *testing.T, drift, failIPv6 bool) (*http.Client, *int) {
	t.Helper()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "www.cloudflare.com" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("fetch lacks total five-second deadline")
		}
		snapshot := cloudflare.Bundled()
		ranges, status := snapshot.IPv4, http.StatusOK
		switch r.URL.Path {
		case "/ips-v4/":
			if drift {
				ranges = append(ranges, "192.0.2.0/24")
			}
		case "/ips-v6/":
			ranges = snapshot.IPv6
			if failIPv6 {
				status = http.StatusServiceUnavailable
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(strings.Join(ranges, "\n"))), Header: make(http.Header)}, nil
	})}
	return client, &calls
}

func TestCheck(t *testing.T) {
	t.Parallel()
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "matches", true: "drift"}[drift], func(t *testing.T) {
			t.Parallel()
			client, calls := fixtureClient(t, drift, false)
			var out bytes.Buffer
			err := run(t.Context(), client, nil, &out)
			if drift {
				if err == nil || !strings.Contains(err.Error(), "snapshot differs") || out.Len() != 0 {
					t.Fatalf("drift: output=%q error=%v", out.String(), err)
				}
			} else if err != nil || !strings.Contains(out.String(), "matches both published lists") {
				t.Fatalf("match: output=%q error=%v", out.String(), err)
			}
			if *calls != 2 {
				t.Fatalf("requests = %d, want both source lists", *calls)
			}
		})
	}
}

func TestGenerateSnapshot(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "second source fails"}[fail], func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "snapshot.json")
			before := cloudflare.Bundled()
			if err := writeSnapshot(path, before); err != nil {
				t.Fatal(err)
			}
			original := readSnapshot(t, path)
			client, _ := fixtureClient(t, true, fail)
			var out bytes.Buffer
			err := run(t.Context(), client, []string{"-update", "-output", path}, &out)
			if (err != nil) != fail {
				t.Fatalf("run error = %v, want failure=%v", err, fail)
			}
			data := readSnapshot(t, path)
			if fail {
				if !bytes.Equal(original, data) || out.Len() != 0 {
					t.Fatal("partial fetch changed artifact or reported success")
				}
				return
			}
			assertGeneratedSnapshot(t, data, before, out.String())
			assertOnlySnapshot(t, filepath.Dir(path))
		})
	}
}

func assertGeneratedSnapshot(t *testing.T, data []byte, before cloudflare.Snapshot, output string) {
	t.Helper()
	var got cloudflare.Snapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.IPv4, append(before.IPv4, "192.0.2.0/24")) || !slices.Equal(got.IPv6, before.IPv6) || got.FetchedAt.IsZero() {
		t.Fatalf("generated snapshot = %+v", got)
	}
	if bytes.Contains(data, []byte("RefreshAfter")) || !strings.Contains(output, "Rebuild to embed") {
		t.Fatalf("unexpected artifact/output: %s / %s", data, output)
	}
}

func readSnapshot(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertOnlySnapshot(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "snapshot.json" {
		t.Fatalf("temporary artifact leaked: %v, %v", entries, err)
	}
}

func TestWriteSnapshotValidationAndReproducibility(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	snapshot := cloudflare.Bundled()
	if err := writeSnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	original := readSnapshot(t, path)
	if err := writeSnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	invalid := snapshot
	invalid.IPv6 = nil
	if err := writeSnapshot(path, invalid); err == nil {
		t.Fatal("accepted an incomplete pair")
	}
	if !bytes.Equal(original, readSnapshot(t, path)) {
		t.Fatal("repeated write or invalid snapshot changed artifact")
	}
	assertOnlySnapshot(t, filepath.Dir(path))
}

func TestGeneratorErrors(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-unknown"}, {"unexpected"}, {"-update", "-output", filepath.Join(t.TempDir(), "absent", "snapshot.json")}, {"-update", "-output", t.TempDir()}} {
		client, _ := fixtureClient(t, false, false)
		if err := run(t.Context(), client, args, io.Discard); err == nil {
			t.Fatalf("accepted invalid arguments/output %v", args)
		}
	}
}

func TestWriteSnapshotMarshalFailurePreservesArtifact(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	snapshot := cloudflare.Bundled()
	if err := writeSnapshot(path, snapshot); err != nil {
		t.Fatal(err)
	}
	original := readSnapshot(t, path)
	snapshot.FetchedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := writeSnapshot(path, snapshot); err == nil {
		t.Fatal("unencodable timestamp was accepted")
	}
	if !bytes.Equal(original, readSnapshot(t, path)) {
		t.Fatal("encoding failure damaged existing fallback")
	}
	assertOnlySnapshot(t, filepath.Dir(path))
}

func TestPrepareSnapshotFileErrors(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := writeSnapshot(path, cloudflare.Bundled()); err != nil {
		t.Fatal(err)
	}
	original := readSnapshot(t, path)
	readOnly, err := os.Open(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	if err := prepareSnapshot(readOnly, []byte("incomplete")); err == nil {
		t.Fatal("write to read-only descriptor succeeded")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepareSnapshot(readOnly, []byte("incomplete")); err == nil {
		t.Fatal("closed descriptor succeeded")
	}
	if !bytes.Equal(original, readSnapshot(t, path)) {
		t.Fatal("failed write damaged the previous fallback")
	}
}
