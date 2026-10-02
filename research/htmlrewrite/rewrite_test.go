package htmlrewrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func testEngine(t testing.TB) *engine {
	t.Helper()
	e, err := newEngine(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.close(); err != nil {
			t.Error(err)
		}
	})
	return e
}

func rewrite(e *engine, input []byte, chunk int) ([]byte, error) {
	output, _, err := rewriteMode(e, input, chunk, true)
	return output, err
}

func rewriteMode(e *engine, input []byte, chunk int, buffered bool) ([]byte, int, error) {
	var output bytes.Buffer
	s, err := e.newStreamMode(context.Background(), &output, 8<<20, buffered)
	if err != nil {
		return nil, 0, err
	}
	defer s.close()
	for len(input) > 0 {
		n := min(chunk, len(input))
		if err := s.write(input[:n]); err != nil {
			return nil, s.sink.calls, err
		}
		input = input[n:]
	}
	if err := s.finish(); err != nil {
		return nil, s.sink.calls, err
	}
	return output.Bytes(), s.sink.calls, nil
}

func TestStreamingTransform(t *testing.T) {
	e := testEngine(t)
	var output bytes.Buffer
	s, err := e.newStream(context.Background(), &output, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.write([]byte(`<p>before</p><a class="rewrite" href="old">link</a>`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `href="https://example.invalid/rewritten"`) ||
		!strings.Contains(output.String(), `<em>inserted</em>`) {
		t.Fatalf("no transformed output before EOF: %q", output.String())
	}
	if err := s.write([]byte(`<div class="remove">secret</div><p>after</p>`)); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "secret") || !strings.HasSuffix(output.String(), "<p>after</p>") {
		t.Fatalf("unexpected output: %s", &output)
	}
	if err := s.write([]byte("late")); err == nil {
		t.Fatal("accepted write after finish")
	}
}

func TestNativeParityAndChunkBoundaries(t *testing.T) {
	e := testEngine(t)
	inputs := []string{
		`<a class="rewrite" href="old">é🍉 &amp; text</a><b class="remove">gone</b>`,
		`<!doctype html><p>plain<!--comment--><i>unterminated`,
		`<div class="remove"><a class="rewrite">nested</a></div><a class="rewrite">x</a>`,
		`<script>var x = "<a class='rewrite'>";</script><textarea>&amp;</textarea>`,
		"",
		string(benchmarkInput("dense", 1024)),
		string(benchmarkInput("sparse", 1024)),
	}
	for i, input := range inputs {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, "./guest/target/release/statute-htmlrewrite-spike", "4096")
			command.Stdin = strings.NewReader(input)
			want, err := command.Output()
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					t.Logf("native stderr: %s", exitErr.Stderr)
				}
				t.Fatalf("native oracle: %v", err)
			}
			for _, chunk := range []int{1, 2, 7, 4096, chunkSize + 1} {
				for _, buffered := range []bool{false, true} {
					got, _, err := rewriteMode(e, []byte(input), chunk, buffered)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("chunk %d buffered %v: error %v, got %q, want %q", chunk, buffered, err, got, want)
					}
				}
			}
		})
	}
}

func TestParserMemoryLimit(t *testing.T) {
	e := testEngine(t)
	s, err := e.newStream(context.Background(), io.Discard, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	input := []byte(`<a class="rewrite" title="` + strings.Repeat("x", 2<<20))
	if err := s.write(input); err == nil || !s.module.IsClosed() {
		t.Fatalf("oversized token: error %v, closed %v", err, s.module.IsClosed())
	}
}

func TestLargeInput(t *testing.T) {
	e := testEngine(t)
	input := []byte(strings.Repeat("plain text ", 2*chunkSize))
	for _, chunk := range []int{chunkSize - 1, chunkSize + 1, len(input)} {
		got, err := rewrite(e, input, chunk)
		if err != nil || !bytes.Equal(got, input) {
			t.Fatalf("chunk %d: output length %d, error %v", chunk, len(got), err)
		}
	}
}

func TestFailureAfterEmittingPrefix(t *testing.T) {
	e := testEngine(t)
	var output bytes.Buffer
	prefix := "<p>already emitted</p>"
	s, err := e.newStream(context.Background(), &output, len(prefix))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.write([]byte(prefix)); err != nil || output.String() != prefix {
		t.Fatalf("prefix: %q, %v", &output, err)
	}
	if err := s.write([]byte(`<a class="rewrite">overflow</a>`)); err == nil {
		t.Fatal("accepted output beyond the remaining budget")
	}
	if err := s.finish(); err == nil || output.String() != prefix {
		t.Fatalf("failed rewrite emitted a suffix: %q, %v", &output, err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, nil }

func TestLimitsAndCancellation(t *testing.T) {
	e := testEngine(t)
	for _, tc := range []struct {
		name   string
		writer io.Writer
		limit  int
		cancel bool
	}{
		{"output", io.Discard, 3, false},
		{"writer", failWriter{}, 4096, false},
		{"short writer", shortWriter{}, 4096, false},
		{"cancel", io.Discard, 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := e.newStream(ctx, tc.writer, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			if tc.cancel {
				cancel()
			}
			err = s.write([]byte("<p>long enough</p>"))
			if err == nil || !s.module.IsClosed() {
				t.Fatalf("error %v, closed %v", err, s.module.IsClosed())
			}
			if tc.name == "writer" && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("lost writer error: %v", err)
			}
			if tc.name == "short writer" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("lost short write error: %v", err)
			}
			if err := s.write([]byte("again")); err == nil {
				t.Fatal("poisoned instance accepted more input")
			}
		})
	}
	if got, err := rewrite(e, []byte("<p>fresh</p>"), 1); err != nil || string(got) != "<p>fresh</p>" {
		t.Fatalf("failed instance poisoned shared engine: %q, %v", got, err)
	}
}

func TestInstanceLifecycle(t *testing.T) {
	e := testEngine(t)
	for _, tc := range []struct {
		writer io.Writer
		limit  int
	}{{nil, 4096}, {io.Discard, -1}} {
		if _, err := e.newStream(context.Background(), tc.writer, tc.limit); err == nil {
			t.Fatal("accepted invalid sink")
		}
	}
	s, err := e.newStream(context.Background(), io.Discard, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, ok := s.module.Memory().Grow(512); ok {
		t.Fatal("linear memory grew beyond the 512-page limit")
	}
	if err := e.close(); err != nil {
		t.Fatal(err)
	}
	if !s.module.IsClosed() {
		t.Fatal("engine shutdown left an instance open")
	}
	if err := s.finish(); err == nil {
		t.Fatal("accepted finish after shutdown")
	}
	if _, err := e.newStream(context.Background(), io.Discard, 4096); err == nil {
		t.Fatal("accepted instance after shutdown")
	}
}

func TestGuestRejectsInvalidLifecycle(t *testing.T) {
	e := testEngine(t)
	for _, tc := range []struct {
		name string
		args []uint64
	}{{"create", []uint64{1}}, {"write", []uint64{chunkSize + 1}}} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := e.newStream(context.Background(), io.Discard, 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			if err := s.invoke(tc.name, tc.args...); err == nil || !s.module.IsClosed() {
				t.Fatalf("invalid call: error %v, closed %v", err, s.module.IsClosed())
			}
		})
	}
}

func TestConcurrentIsolation(t *testing.T) {
	e := testEngine(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			input := fmt.Sprintf("<p>instance-%d</p>", i)
			got, err := rewrite(e, []byte(input), 1)
			if err != nil || string(got) != input {
				t.Errorf("instance %d: %q, %v", i, got, err)
			}
		})
	}
	wg.Wait()
}

func TestCapabilities(t *testing.T) {
	e := testEngine(t)
	imports := e.compiled.ImportedFunctions()
	if len(imports) != 1 {
		t.Fatalf("unexpected imports: %v", imports)
	}
	module, name, ok := imports[0].Import()
	if !ok || module != "sink" || name != "emit" || len(e.compiled.ImportedMemories()) != 0 {
		t.Fatalf("unexpected capabilities: %s.%s", module, name)
	}
	t.Logf("Wasm artifact: %d bytes; sole import: %s.%s", len(guest), module, name)
}

func BenchmarkRewrite(b *testing.B) {
	e := testEngine(b)
	for _, count := range []int{16, 1024} {
		input := []byte(strings.Repeat(`<a class="rewrite" href="old">payload</a>`, count))
		b.Run(fmt.Sprintf("bytes=%d", len(input)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for b.Loop() {
				if _, err := rewrite(e, input, 4096); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkPassthrough(b *testing.B) {
	for _, count := range []int{16, 1024} {
		input := []byte(strings.Repeat(`<a class="rewrite" href="old">payload</a>`, count))
		b.Run(fmt.Sprintf("bytes=%d", len(input)), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			for b.Loop() {
				var output bytes.Buffer
				if _, err := output.Write(input); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkColdEngine(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		e, err := newEngine(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if err := e.close(); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzChunkBoundaries(f *testing.F) {
	e := testEngine(f)
	f.Add([]byte(`<a class="rewrite">é &amp; x</a><b class="remove">hide</b>`), uint16(1))
	f.Add([]byte(`<select><xmp><script>malformed`), uint16(7))
	f.Fuzz(func(t *testing.T, input []byte, size uint16) {
		input = input[:min(len(input), 8192)]
		want, baselineErr := rewrite(e, input, chunkSize)
		for _, buffered := range []bool{false, true} {
			got, _, err := rewriteMode(e, input, int(size)%256+1, buffered)
			if (err == nil) != (baselineErr == nil) || (err == nil && !bytes.Equal(got, want)) {
				t.Fatalf("chunking changed result: %q / %v; baseline %q / %v", got, err, want, baselineErr)
			}
		}
	})
}
