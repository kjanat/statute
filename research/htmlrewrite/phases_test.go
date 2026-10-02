package htmlrewrite

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// This observer measures the full host emit function, including range checks,
// the output copy, and the writer. It is only used by serialized probe calls.
type emitTimer struct {
	start   time.Time
	elapsed time.Duration
	calls   int
}

func (p *emitTimer) NewFunctionListener(def api.FunctionDefinition) experimental.FunctionListener {
	if def.ModuleName() == "sink" && def.Name() == "emit" {
		return p
	}
	return nil
}

func (p *emitTimer) Before(context.Context, api.Module, api.FunctionDefinition, []uint64, experimental.StackIterator) {
	p.start = time.Now()
}

func (p *emitTimer) After(context.Context, api.Module, api.FunctionDefinition, []uint64) {
	p.elapsed += time.Since(p.start)
	p.calls++
}

func (p *emitTimer) Abort(context.Context, api.Module, api.FunctionDefinition, error) {
	p.elapsed += time.Since(p.start)
	p.calls++
}

func TestEmitTimer(t *testing.T) {
	var probe emitTimer
	ctx := experimental.WithFunctionListenerFactory(context.Background(), &probe)
	e, err := newEngine(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	for _, writer := range []io.Writer{io.Discard, failWriter{}} {
		s, err := e.newStream(ctx, writer, 4096)
		if err != nil {
			t.Fatal(err)
		}
		before := probe.calls
		err = s.write([]byte("hello"))
		if writer == io.Discard && err != nil {
			t.Fatal(err)
		}
		if writer != io.Discard && err == nil {
			t.Fatal("probe swallowed writer failure")
		}
		if probe.calls != before+1 || probe.elapsed <= 0 {
			t.Fatalf("emit observer missed call: %+v", probe)
		}
		_ = s.close()
	}
}

func BenchmarkPhases(b *testing.B) {
	for _, shape := range []string{"dense", "sparse"} {
		b.Run(shape, func(b *testing.B) {
			var probe emitTimer
			ctx := experimental.WithFunctionListenerFactory(context.Background(), &probe)
			e, err := newEngine(ctx)
			if err != nil {
				b.Fatal(err)
			}
			defer e.close()
			input := benchmarkInput(shape, 1024)
			var setup, write, finish time.Duration
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for b.Loop() {
				start := time.Now()
				s, err := e.newStream(ctx, io.Discard, 8<<20)
				setup += time.Since(start)
				if err != nil {
					b.Fatal(err)
				}
				start = time.Now()
				for offset := 0; offset < len(input); offset += 4096 {
					if err := s.write(input[offset:min(offset+4096, len(input))]); err != nil {
						_ = s.close()
						b.Fatal(err)
					}
				}
				write += time.Since(start)
				start = time.Now()
				if err := s.finish(); err != nil {
					b.Fatal(err)
				}
				finish += time.Since(start)
			}
			for name, value := range map[string]time.Duration{
				"setup-ns/op": setup, "write-ns/op": write,
				"finish-ns/op": finish, "emit-ns/op": probe.elapsed,
			} {
				b.ReportMetric(float64(value.Nanoseconds())/float64(b.N), name)
			}
			b.ReportMetric(float64(probe.calls)/float64(b.N), "callbacks/op")
		})
	}
}

// The disabled case is a diagnostic control on a fixed, finite fixture. It is
// not an alternative runtime policy: it cannot terminate arbitrary guest work.
func BenchmarkCancellationCost(b *testing.B) {
	for _, enabled := range []bool{true, false} {
		name := "enabled"
		if !enabled {
			name = "disabled-unsafe-control"
		}
		b.Run(name, func(b *testing.B) {
			e, err := newEngineConfigured(context.Background(), wazero.NewRuntimeConfigCompiler().
				WithMemoryLimitPages(512).WithCloseOnContextDone(enabled))
			if err != nil {
				b.Fatal(err)
			}
			defer e.close()
			input := benchmarkInput("dense", 1024)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := rewrite(e, input, 4096); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
