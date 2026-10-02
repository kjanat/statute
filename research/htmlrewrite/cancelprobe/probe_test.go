package cancelprobe

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

type readyKey struct{}

type probe struct {
	runtime     wazero.Runtime
	compiled    wazero.CompiledModule
	cancellable bool
}

func newProbe(t testing.TB, batch int, cancellable bool) *probe {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithMemoryLimitPages(1).WithCloseOnContextDone(cancellable))
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	_, err := r.NewHostModuleBuilder("probe").NewFunctionBuilder().
		WithGoFunction(api.GoFunc(func(ctx context.Context, _ []uint64) {
			close(ctx.Value(readyKey{}).(chan struct{}))
		}), nil, nil).Export("ready").Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := r.CompileModule(ctx, fixture(batch))
	if err != nil {
		t.Fatal(err)
	}
	return &probe{r, compiled, cancellable}
}

func (p *probe) instance(t testing.TB) api.Module {
	t.Helper()
	m, err := p.runtime.InstantiateModule(context.Background(), p.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func (p *probe) spin(ctx context.Context, m api.Module) error {
	if !p.cancellable {
		return errors.New("infinite probe requires termination checks")
	}
	_, err := m.ExportedFunction("spin").Call(ctx)
	return err
}

func TestEquivalentWork(t *testing.T) {
	for _, batch := range []int{1, 16, 256} {
		for _, enabled := range []bool{true, false} {
			t.Run(fmt.Sprintf("batch=%d/cancel=%v", batch, enabled), func(t *testing.T) {
				p := newProbe(t, batch, enabled)
				m := p.instance(t)
				fn := m.ExportedFunction("work")
				for _, groups := range []int{0, 1, 7, 256} {
					for _, seed := range []uint32{0, 1, 42, 0xffffffff} {
						result, err := fn.Call(context.Background(), uint64(groups), uint64(seed))
						if err != nil || len(result) != 1 || uint32(result[0]) != reference(groups*batch, seed) {
							t.Fatalf("groups %d seed %d: %v, %v", groups, seed, result, err)
						}
					}
				}
				if !enabled && p.spin(context.Background(), m) == nil {
					t.Fatal("allowed an uninterruptible infinite probe")
				}
			})
		}
	}
}

func TestCapabilities(t *testing.T) {
	p := newProbe(t, 1, true)
	imports := p.compiled.ImportedFunctions()
	if len(imports) != 1 || len(p.compiled.ImportedMemories()) != 0 || len(p.compiled.ExportedMemories()) != 0 {
		t.Fatal("unexpected probe capabilities")
	}
	module, name, ok := imports[0].Import()
	if !ok || module != "probe" || name != "ready" {
		t.Fatal("unexpected host import")
	}
}

// Every sample owns its runtime and invocation. Cancellation starts after a
// guest warm-up signal and a 1-ms observation window with the call outstanding.
type interruptionSample struct {
	triggerLag time.Duration
	stop       time.Duration
}

func interruption(t *testing.T, batch int, mode string) interruptionSample {
	t.Helper()
	p := newProbe(t, batch, true)
	m := p.instance(t)
	sibling := p.instance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	ctx = context.WithValue(ctx, readyKey{}, ready)
	done := make(chan error, 1)
	go func() { done <- p.spin(ctx, m) }()
	joined := false
	defer func() {
		cancel()
		_ = p.runtime.Close(context.Background())
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("probe did not join during failure cleanup")
			}
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		joined = true
		t.Fatalf("guest exited before warm-up: %v", err)
	case <-ctx.Done():
		t.Fatal("guest warm-up timed out")
	}
	window := time.Now()
	select {
	case err := <-done:
		joined = true
		t.Fatalf("guest stopped without interruption: %v", err)
	case <-time.After(time.Millisecond):
	}
	start := time.Now()
	lag := start.Sub(window) - time.Millisecond
	switch mode {
	case "context":
		cancel()
	case "module":
		if err := m.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	case "runtime":
		if err := p.runtime.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown interruption mode")
	}
	select {
	case err := <-done:
		joined = true
		elapsed := time.Since(start)
		if err == nil || !m.IsClosed() {
			t.Fatalf("interruption failed to close instance: %v", err)
		}
		if mode == "context" && !errors.Is(err, context.Canceled) {
			t.Fatalf("wrong cancellation cause: %v", err)
		}
		if _, err := m.ExportedFunction("work").Call(context.Background(), 1, 1); err == nil {
			t.Fatal("closed instance accepted work")
		}
		if mode != "runtime" {
			out, err := sibling.ExportedFunction("work").Call(context.Background(), 1, 1)
			if err != nil || len(out) != 1 || uint32(out[0]) != reference(batch, 1) || sibling.ExportedGlobal("state").Get() != 1 {
				t.Fatalf("interruption poisoned sibling: %v, %v", out, err)
			}
		} else if !sibling.IsClosed() {
			t.Fatal("runtime shutdown left sibling open")
		}
		return interruptionSample{lag, elapsed}
	case <-time.After(5 * time.Second):
		t.Fatal("active guest did not terminate")
		return interruptionSample{}
	}
}

func TestActiveInterruption(t *testing.T) {
	for _, procs := range []int{1, 4} {
		t.Run(fmt.Sprintf("procs=%d", procs), func(t *testing.T) {
			previous := runtime.GOMAXPROCS(procs)
			defer runtime.GOMAXPROCS(previous)
			for _, batch := range []int{1, 16, 256} {
				for _, mode := range []string{"context", "module", "runtime"} {
					t.Run(fmt.Sprintf("batch=%d/%s", batch, mode), func(t *testing.T) {
						var durations []time.Duration
						var lags []time.Duration
						for range 20 {
							sample := interruption(t, batch, mode)
							durations = append(durations, sample.stop)
							lags = append(lags, sample.triggerLag)
						}
						slices.Sort(durations)
						slices.Sort(lags)
						t.Logf("n=%d stop_p50=%s stop_p95=%s stop_max=%s trigger_lag_p50=%s trigger_lag_max=%s", len(durations), durations[9], durations[18], durations[19], lags[9], lags[19])
					})
				}
			}
		})
	}
}

func BenchmarkLoop(b *testing.B) {
	const total = 65536
	want := reference(total, 42)
	for _, batch := range []int{1, 16, 256} {
		for _, enabled := range []bool{true, false} {
			b.Run(fmt.Sprintf("batch=%d/cancel=%v", batch, enabled), func(b *testing.B) {
				p := newProbe(b, batch, enabled)
				m := p.instance(b)
				fn := m.ExportedFunction("work")
				b.ReportAllocs()
				for b.Loop() {
					out, err := fn.Call(context.Background(), total/uint64(batch), 42)
					if err != nil || len(out) != 1 || uint32(out[0]) != want {
						b.Fatalf("work mismatch: %v, %v", out, err)
					}
				}
				b.ReportMetric(total, "steps/op")
				b.ReportMetric(float64(len(fixture(batch))), "wasm-bytes")
			})
		}
	}
}
