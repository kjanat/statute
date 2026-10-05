package htmlrewrite

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestOptimizerArtifact(t *testing.T) {
	t.Logf("optimizer_guest sha256=%x", sha256.Sum256(guest))
}

// Measure cancellation of a continuously fed real parser, including scheduling
// delay before the cancellation request. Calls can cross chunk boundaries.
func TestOptimizerCancellation(t *testing.T) {
	for _, procs := range []int{1, 4} {
		t.Run(fmt.Sprintf("procs=%d", procs), func(t *testing.T) {
			previous := runtime.GOMAXPROCS(procs)
			defer runtime.GOMAXPROCS(previous)
			e := testEngine(t)
			input := benchmarkInput("dense", 1500)
			var joins, lags []time.Duration
			for range 20 {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				entered := make(chan struct{})
				var once sync.Once
				s, err := e.newStream(ctx, writerFunc(func(b []byte) (int, error) {
					once.Do(func() { close(entered) })
					return len(b), nil
				}), 1<<30)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					for {
						if err := s.write(input); err != nil {
							done <- err
							return
						}
					}
				}()
				select {
				case <-entered:
				case err := <-done:
					cancel()
					_ = s.close()
					t.Fatalf("parser failed before output: %v", err)
				}
				due := time.Now().Add(time.Millisecond)
				time.Sleep(time.Until(due))
				requested := time.Now()
				expired := ctx.Err()
				cancel()
				err = <-done
				joins = append(joins, time.Since(requested))
				lags = append(lags, requested.Sub(due))
				if expired != nil || !errors.Is(err, context.Canceled) || !s.module.IsClosed() {
					t.Fatalf("parser cancellation: expired=%v, result=%v, closed=%v", expired, err, s.module.IsClosed())
				}
				if err := s.write([]byte("again")); err == nil {
					t.Fatal("cancelled parser accepted more input")
				}
				_ = s.close()
			}
			if got, err := rewrite(e, []byte("<p>sibling</p>"), 1); err != nil || string(got) != "<p>sibling</p>" {
				t.Fatalf("cancellation poisoned shared engine: %q, %v", got, err)
			}
			t.Logf("optimizer_cancel sha256=%x procs=%d samples=%d join_p50_ns=%d join_max_ns=%d trigger_lag_p50_ns=%d trigger_lag_max_ns=%d",
				sha256.Sum256(guest), procs, len(joins), loadQuantile(joins, .5), loadQuantile(joins, 1), loadQuantile(lags, .5), loadQuantile(lags, 1))
		})
	}
}

func BenchmarkOptimizer(b *testing.B) {
	for _, shape := range []string{"dense", "sparse"} {
		for _, count := range []int{16, 1024, 32768} {
			b.Run(fmt.Sprintf("%s/%d", shape, count), func(b *testing.B) {
				e := testEngine(b)
				input := benchmarkInput(shape, count)
				b.ReportAllocs()
				b.SetBytes(int64(len(input)))
				for b.Loop() {
					s, err := e.newStream(b.Context(), io.Discard, 32<<20)
					if err != nil {
						b.Fatal(err)
					}
					for offset := 0; offset < len(input); offset += 4096 {
						if err := s.write(input[offset:min(offset+4096, len(input))]); err != nil {
							_ = s.close()
							b.Fatal(err)
						}
					}
					if err := s.finish(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
