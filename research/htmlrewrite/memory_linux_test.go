//go:build statute_htmlrewrite

package htmlrewrite

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	loadDuration = flag.Duration("load-duration", 100*time.Millisecond, "duration of each memory-load round")
	loadRounds   = flag.Int("load-rounds", 1, "memory-load rounds per engine")
	loadWorkers  = flag.Int("load-workers", 2, "concurrent memory-load streams")
	loadCount    = flag.Int("load-count", 1024, "HTML fixture fragments (1..131072)")
	loadShape    = flag.String("load-shape", "dense", "dense or sparse selector matches")
)

type memorySample struct {
	HeapAlloc    uint64 `json:"heap_alloc"`
	HeapInuse    uint64 `json:"heap_inuse"`
	HeapReleased uint64 `json:"heap_released"`
	TotalAlloc   uint64 `json:"total_alloc"`
	NumGC        uint32 `json:"num_gc"`
	Goroutines   int    `json:"goroutines"`
	RSS          uint64 `json:"rss"`
	HWM          uint64 `json:"process_hwm"`
}

func loadBuildSettings(t testing.TB) []debug.BuildSetting {
	t.Helper()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("Go build provenance unavailable")
	}
	return info.Settings
}

func processMemory(data string) (rss, hwm uint64, err error) {
	found := 0
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || (fields[0] != "VmRSS:" && fields[0] != "VmHWM:") {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, 0, fmt.Errorf("invalid process memory line %q", line)
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || value > ^uint64(0)/1024 {
			return 0, 0, fmt.Errorf("invalid process memory value %q", fields[1])
		}
		if fields[0] == "VmRSS:" {
			rss = value * 1024
			found |= 1
		} else {
			hwm = value * 1024
			found |= 2
		}
	}
	if found != 3 {
		return 0, 0, errors.New("process RSS/high-water mark unavailable")
	}
	return rss, hwm, nil
}

func sampleMemory() (memorySample, error) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return memorySample{}, err
	}
	rss, hwm, err := processMemory(string(data))
	return memorySample{m.HeapAlloc, m.HeapInuse, m.HeapReleased, m.TotalAlloc, m.NumGC, runtime.NumGoroutine(), rss, hwm}, err
}

type loadResult struct {
	Completed   uint64        `json:"completed"`
	Interrupted uint64        `json:"interrupted"`
	Elapsed     time.Duration `json:"elapsed_ns"`
	PeakHeap    uint64        `json:"sampled_peak_heap"`
	PeakRSS     uint64        `json:"sampled_peak_rss"`
	Samples     int           `json:"samples"`
}

func memoryLoad(e *engine, input []byte, workers int, duration time.Duration) (loadResult, error) {
	if workers < 1 || workers > 64 || duration <= 0 {
		return loadResult{}, errors.New("load requires 1..64 workers and positive duration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()
	var completed, interrupted atomic.Uint64
	var wg sync.WaitGroup
	errorsOut := make(chan error, workers)
	start := time.Now()
	for range workers {
		wg.Go(func() {
			for ctx.Err() == nil {
				err := loadRewrite(ctx, e, input)
				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						interrupted.Add(1)
					} else {
						errorsOut <- err
						cancel()
					}
					return
				}
				completed.Add(1)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	// Join workers even if a measurement fails. Cancellation interrupts guest
	// execution; the discard sink does not introduce a blocking Go callback.
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var result loadResult
	for {
		m, err := sampleMemory()
		if err != nil {
			return result, err
		}
		result.Samples++
		result.PeakHeap = max(result.PeakHeap, m.HeapAlloc)
		result.PeakRSS = max(result.PeakRSS, m.RSS)
		select {
		case <-done:
			result.Completed, result.Interrupted = completed.Load(), interrupted.Load()
			result.Elapsed = time.Since(start)
			close(errorsOut)
			var failures []error
			for err := range errorsOut {
				failures = append(failures, err)
			}
			return result, errors.Join(failures...)
		case <-ticker.C:
		}
	}
}

func loadRewrite(ctx context.Context, e *engine, input []byte) error {
	s, err := e.newStream(ctx, io.Discard, 32<<20)
	if err != nil {
		return err
	}
	defer s.close()
	for offset := 0; offset < len(input); offset += 4096 {
		if err := s.write(input[offset:min(offset+4096, len(input))]); err != nil {
			return err
		}
	}
	return s.finish()
}

func TestMemoryLoad(t *testing.T) {
	if *loadRounds < 1 || *loadRounds > 100 || *loadWorkers < 1 || *loadWorkers > 64 || *loadDuration <= 0 {
		t.Fatal("load requires 1..100 rounds, 1..64 workers, and positive duration")
	}
	if *loadCount < 1 || *loadCount > 131072 || (*loadShape != "dense" && *loadShape != "sparse") {
		t.Fatal("load requires 1..131072 fragments and dense or sparse shape")
	}
	logJSON := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %s", name, data)
	}
	snapshot := func(name string) {
		t.Helper()
		m, err := sampleMemory()
		if err != nil {
			t.Fatal(err)
		}
		logJSON(name, m)
	}
	input := benchmarkInput(*loadShape, *loadCount)
	logJSON("config", map[string]any{
		"go": runtime.Version(), "arch": runtime.GOARCH, "gomaxprocs": runtime.GOMAXPROCS(0),
		"workers": *loadWorkers, "rounds": *loadRounds, "duration_ns": *loadDuration,
		"input_bytes": len(input), "guest_sha256": fmt.Sprintf("%x", sha256.Sum256(guest)),
		"shape": *loadShape, "output_limit": 32 << 20,
		"build_settings": loadBuildSettings(t),
	})
	debug.FreeOSMemory()
	snapshot("before_engine")
	e, err := newEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if e != nil {
			_ = e.close()
		}
	}()
	runtime.GC()
	snapshot("compiled_idle")
	for round := 1; round <= *loadRounds; round++ {
		result, err := memoryLoad(e, input, *loadWorkers, *loadDuration)
		if err != nil {
			t.Fatal(err)
		}
		logJSON(fmt.Sprintf("round_%d", round), result)
		// No forced GC or scavenging occurs while the load is running.
		runtime.GC()
		snapshot(fmt.Sprintf("round_%d_post_gc", round))
	}
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	e = nil
	runtime.GC()
	snapshot("closed_post_gc")
	debug.FreeOSMemory()
	snapshot("closed_scavenged")
}

func TestProcessMemory(t *testing.T) {
	rss, hwm, err := processMemory("Name: probe\nVmRSS:\t12 kB\nVmHWM: 34 kB\n")
	if err != nil || rss != 12*1024 || hwm != 34*1024 {
		t.Fatalf("memory: %d %d %v", rss, hwm, err)
	}
	for _, data := range []string{"", "VmRSS: 12 kB", "VmRSS: -1 kB\nVmHWM: 1 kB", "VmRSS: 1 MB\nVmHWM: 1 kB", "VmRSS: 18446744073709551615 kB\nVmHWM: 1 kB"} {
		if _, _, err := processMemory(data); err == nil {
			t.Fatalf("accepted invalid memory fields %q", data)
		}
	}
}

func TestMemoryLoadFailure(t *testing.T) {
	e := testEngine(t)
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryLoad(e, []byte("hello"), 2, time.Second); err == nil {
		t.Fatal("load swallowed a closed-engine failure")
	}
	for _, workers := range []int{0, 65} {
		if _, err := memoryLoad(e, nil, workers, time.Second); err == nil {
			t.Fatal("accepted invalid concurrency")
		}
	}
}
