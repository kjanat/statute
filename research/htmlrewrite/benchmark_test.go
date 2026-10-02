package htmlrewrite

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func benchmarkInput(shape string, count int) []byte {
	var input strings.Builder
	for i := range count {
		if shape == "dense" || i%32 == 0 {
			input.WriteString(`<a class="rewrite" href="old">payload</a>`)
		} else {
			input.WriteString(`<p>plain text without a matching class</p>`)
		}
	}
	return []byte(input.String())
}

func BenchmarkMatrix(b *testing.B) {
	e := testEngine(b)
	for _, shape := range []string{"dense", "sparse"} {
		for _, count := range []int{16, 1024} {
			input := benchmarkInput(shape, count)
			for _, chunk := range []int{1, 4096} {
				for _, buffered := range []bool{false, true} {
					b.Run(fmt.Sprintf("shape=%s/bytes=%d/chunk=%d/buffered=%v", shape, len(input), chunk, buffered), func(b *testing.B) {
						b.ReportAllocs()
						b.SetBytes(int64(len(input)))
						calls := 0
						for b.Loop() {
							_, n, err := rewriteMode(e, input, chunk, buffered)
							if err != nil {
								b.Fatal(err)
							}
							calls += n
						}
						b.ReportMetric(float64(calls)/float64(b.N), "callbacks/op")
					})
				}
			}
		}
	}
}

func BenchmarkInstance(b *testing.B) {
	e := testEngine(b)
	b.ReportAllocs()
	for b.Loop() {
		s, err := e.newStream(context.Background(), io.Discard, 0)
		if err != nil {
			b.Fatal(err)
		}
		if err := s.close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParallel(b *testing.B) {
	e := testEngine(b)
	input := []byte(strings.Repeat(`<a class="rewrite" href="old">payload</a>`, 1024))
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := rewrite(e, input, 4096); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkFirstOutput(b *testing.B) {
	e := testEngine(b)
	input := []byte(strings.Repeat(`<a class="rewrite" href="old">payload</a>`, 1024))
	var total time.Duration
	b.ReportAllocs()
	for b.Loop() {
		start := time.Now()
		var first time.Duration
		s, err := e.newStream(context.Background(), writerFunc(func(bytes []byte) (int, error) {
			if first == 0 {
				first = time.Since(start)
			}
			return len(bytes), nil
		}), 8<<20)
		if err != nil {
			b.Fatal(err)
		}
		for offset := 0; offset < len(input); offset += 4096 {
			if err := s.write(input[offset:min(offset+4096, len(input))]); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.finish(); err != nil {
			b.Fatal(err)
		}
		if first == 0 {
			b.Fatal("no output")
		}
		total += first
	}
	b.ReportMetric(float64(total.Nanoseconds())/float64(b.N), "first-byte-ns/op")
}
