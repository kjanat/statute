//go:build statute_htmlrewrite

package htmlrewrite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

var (
	httpLoadDuration = flag.Duration("http-load-duration", 100*time.Millisecond, "HTTP load admission window")
	httpLoadCount    = flag.Int("http-load-count", 1024, "HTML fixture fragment count (up to 131072)")
	httpLoadShape    = flag.String("http-load-shape", "dense", "dense or sparse selector matches")
	httpLoadWorkers  = flag.Int("http-load-workers", 2, "concurrent clients (1..4)")
	httpLoadMode     = flag.String("http-load-mode", "stream", "plain, stream, or etag")
	httpLoadPace     = flag.Duration("http-load-pace", 0, "delay per 16 KiB consumed; zero reads at full speed")
)

type httpLoadObservation struct {
	First time.Duration
	Total time.Duration
	Bytes int64
}

// Consumption pacing applies to bytes, independently of TCP Read boundaries.
func observeHTTP(ctx context.Context, client *http.Client, endpoint string, want [32]byte, pace time.Duration) (httpLoadObservation, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return httpLoadObservation{}, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := client.Do(req)
	if err != nil {
		return httpLoadObservation{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return httpLoadObservation{}, fmt.Errorf("HTTP status %d", res.StatusCode)
	}
	if req.URL.Path == "/etag" && res.Header.Get("ETag") == "" {
		return httpLoadObservation{}, errors.New("buffered route omitted its ETag")
	}
	var observation httpLoadObservation
	hash := sha256.New()
	buffer := make([]byte, 16<<10)
	readStart := time.Now()
	for {
		n, readErr := res.Body.Read(buffer)
		if n > 0 {
			if observation.Bytes == 0 {
				observation.First = time.Since(start)
			}
			observation.Bytes += int64(n)
			_, _ = hash.Write(buffer[:n])
			if pace > 0 {
				due := readStart.Add(time.Duration(observation.Bytes) * pace / (16 << 10))
				timer := time.NewTimer(max(0, time.Until(due)))
				select {
				case <-ctx.Done():
					timer.Stop()
					return observation, ctx.Err()
				case <-timer.C:
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return observation, readErr
		}
	}
	observation.Total = time.Since(start)
	if !bytes.Equal(hash.Sum(nil), want[:]) {
		return observation, errors.New("response checksum mismatch")
	}
	return observation, nil
}

func loadQuantile(values []time.Duration, q float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	return ordered[max(0, int(math.Ceil(q*float64(len(ordered))))-1)]
}

// Warm-up and recovery must finish verification before their memory boundary.
func verifiedHTTPMemory(verify func() error, memory func() (uint64, uint64, error)) (uint64, uint64, error) {
	if err := verify(); err != nil {
		return 0, 0, err
	}
	return memory()
}

func TestHTTPLoad(t *testing.T) {
	if *httpLoadDuration <= 0 || *httpLoadDuration > 5*time.Minute || *httpLoadCount < 1 || *httpLoadCount > 131072 ||
		*httpLoadWorkers < 1 || *httpLoadWorkers > 4 || *httpLoadPace < 0 || *httpLoadPace > 32*time.Millisecond ||
		(*httpLoadShape != "dense" && *httpLoadShape != "sparse") || !slices.Contains([]string{"plain", "stream", "etag"}, *httpLoadMode) {
		t.Fatal("invalid HTTP load configuration")
	}
	input := benchmarkInput(*httpLoadShape, *httpLoadCount)
	expected := input
	if *httpLoadMode != "plain" {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "./guest/target/release/statute-htmlrewrite-spike", "4096")
		cmd.Stdin = bytes.NewReader(input)
		var err error
		expected, err = cmd.Output()
		if err != nil {
			t.Fatalf("native oracle: %v", err)
		}
	}
	want := sha256.Sum256(expected)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(input)
	}))
	defer origin.Close()
	endpoint, output := startHTTPStatuteBudget(t, origin.URL, "", []string{"STATUTE_HTML_HTTP_LOAD=1"}, *httpLoadDuration+90*time.Second)
	endpoint += "/" + *httpLoadMode
	transport := &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: *httpLoadWorkers}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 40 * time.Second}
	logJSON := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %s", name, data)
	}
	logJSON("http_config", map[string]any{
		"go": runtime.Version(), "arch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0),
		"guest_sha256": fmt.Sprintf("%x", sha256.Sum256(guest)), "input_bytes": len(input), "output_bytes": len(expected),
		"shape": *httpLoadShape, "mode": *httpLoadMode, "workers": *httpLoadWorkers,
		"admission_ns": *httpLoadDuration, "pace_per_16k_ns": *httpLoadPace,
		"build_settings": loadBuildSettings(t),
	})
	serverMemory := func() (uint64, uint64, error) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", output.pid))
		if err != nil {
			return 0, 0, err
		}
		return processMemory(string(data))
	}
	verify := func() error {
		_, err := observeHTTP(t.Context(), client, endpoint, want, 0)
		return err
	}
	// One verified warm request precedes the baseline and timed load.
	before, _, err := verifiedHTTPMemory(verify, serverMemory)
	if err != nil {
		t.Fatalf("warm-up boundary: %v", err)
	}
	start := time.Now()
	stop := start.Add(*httpLoadDuration)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var observations []httpLoadObservation
	failures := make(chan error, *httpLoadWorkers)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for range *httpLoadWorkers {
		wg.Go(func() {
			for time.Now().Before(stop) && ctx.Err() == nil {
				requestCtx, requestCancel := context.WithTimeout(ctx, 40*time.Second)
				observation, err := observeHTTP(requestCtx, client, endpoint, want, *httpLoadPace)
				requestCancel()
				if err != nil {
					failures <- err
					cancel()
					return
				}
				mu.Lock()
				full := len(observations) >= 1000000
				if !full {
					observations = append(observations, observation)
				}
				mu.Unlock()
				if full {
					failures <- errors.New("latency sample limit reached")
					cancel()
					return
				}
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var peakRSS, hwm uint64
	collect := func() {
		t.Helper()
		rss, high, err := serverMemory()
		if err != nil {
			t.Fatal(err)
		}
		peakRSS, hwm = max(peakRSS, rss), max(hwm, high)
	}
	collect()
sampling:
	for {
		select {
		case <-ticker.C:
			collect()
		case <-done:
			break sampling
		}
	}
	elapsed := time.Since(start)
	collect()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if len(observations) == 0 {
		t.Fatal("no completed requests")
	}
	var first, total []time.Duration
	var delivered int64
	for _, sample := range observations {
		first, total = append(first, sample.First), append(total, sample.Total)
		delivered += sample.Bytes
	}
	after, _, err := serverMemory()
	if err != nil {
		t.Fatal(err)
	}
	// Recovery is outside load timing but inside the final process high-water mark.
	recovered, high, err := verifiedHTTPMemory(verify, serverMemory)
	if err != nil {
		t.Fatalf("post-load serving boundary: %v", err)
	}
	hwm = max(hwm, high)
	logJSON("http_result", map[string]any{
		"completed": len(observations), "elapsed_ns": elapsed, "output_bytes": delivered,
		"requests_per_second": float64(len(observations)) / elapsed.Seconds(),
		"first_body_p50_ns":   loadQuantile(first, .50), "first_body_p95_ns": loadQuantile(first, .95), "first_body_p99_ns": loadQuantile(first, .99),
		"total_p50_ns": loadQuantile(total, .50), "total_p95_ns": loadQuantile(total, .95), "total_p99_ns": loadQuantile(total, .99),
		"server_before_rss": before, "server_sampled_peak_rss": peakRSS, "server_process_hwm": hwm, "server_after_rss": after,
		"server_recovery_rss": recovered,
	})
}

func TestVerifiedHTTPMemory(t *testing.T) {
	for _, failure := range []string{"none", "verification", "memory"} {
		t.Run(failure, func(t *testing.T) {
			var events []string
			fault := errors.New("boundary failure")
			verify := func() error {
				events = append(events, "verified")
				if failure == "verification" {
					return fault
				}
				return nil
			}
			memory := func() (uint64, uint64, error) {
				events = append(events, "sampled")
				if failure == "memory" {
					return 0, 0, fault
				}
				return 100, 200, nil
			}
			rss, hwm, err := verifiedHTTPMemory(verify, memory)
			wantEvents := []string{"verified", "sampled"}
			if failure == "verification" {
				wantEvents = wantEvents[:1]
			}
			if !slices.Equal(events, wantEvents) {
				t.Fatalf("boundary order: %v, want %v", events, wantEvents)
			}
			if failure == "none" {
				if err != nil || rss != 100 || hwm != 200 {
					t.Fatalf("memory: %d/%d, %v", rss, hwm, err)
				}
			} else if !errors.Is(err, fault) || rss != 0 || hwm != 0 {
				t.Fatalf("failed boundary: %d/%d, %v", rss, hwm, err)
			}
		})
	}
}

func TestLoadQuantile(t *testing.T) {
	values := []time.Duration{30, 10, 20, 40}
	if loadQuantile(nil, .95) != 0 || loadQuantile(values, .5) != 20 || loadQuantile(values, .99) != 40 || values[0] != 30 {
		t.Fatal("incorrect nearest-rank quantile or mutated input")
	}
}

func TestObserveHTTPFailures(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); _, _ = w.Write([]byte("wrong")) }))
		_, err := observeHTTP(t.Context(), server.Client(), server.URL, sha256.Sum256([]byte("expected")), 0)
		server.Close()
		if err == nil {
			t.Fatalf("accepted status %d with incorrect body", status)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := observeHTTP(ctx, http.DefaultClient, "http://127.0.0.1:1", [32]byte{}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestObserveHTTPPacingAndTruncation(t *testing.T) {
	input := bytes.Repeat([]byte("x"), 16<<10)
	want := sha256.Sum256(input)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(input)), Header: make(http.Header)}, nil
	})}
	got, err := observeHTTP(t.Context(), client, "http://fixture", want, 20*time.Millisecond)
	if err != nil || got.Bytes != int64(len(input)) || got.Total < 20*time.Millisecond || got.First <= 0 {
		t.Fatalf("paced delivery: %+v, %v", got, err)
	}
	if _, err := observeHTTP(t.Context(), client, "http://fixture/etag", want, 0); err == nil {
		t.Fatal("accepted a buffered route without a validator")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(&cancelLoadReader{bytes.NewReader(input), cancel})}, nil
	})
	if _, err := observeHTTP(ctx, client, "http://fixture", want, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during paced consumption: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer server.Close()
	if _, err := observeHTTP(t.Context(), server.Client(), server.URL, sha256.Sum256([]byte("short")), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated delivery: %v", err)
	}
}

type cancelLoadReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r *cancelLoadReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}
