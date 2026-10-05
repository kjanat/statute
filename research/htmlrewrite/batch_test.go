//go:build statute_htmlrewrite

package htmlrewrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const outputBatchSize = 16 << 10

type recordingWriter struct {
	bytes.Buffer
	sizes []int
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	w.sizes = append(w.sizes, len(b))
	return w.Buffer.Write(b)
}

func TestOutputBatchBoundaries(t *testing.T) {
	e := testEngine(t)
	for _, size := range []int{outputBatchSize - 1, outputBatchSize, outputBatchSize + 1, chunkSize + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var output recordingWriter
			s, err := e.newStream(context.Background(), &output, size)
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			input := strings.Repeat("x", size)
			if err := s.write([]byte(input)); err != nil || output.String() != input {
				t.Fatalf("output not flushed before EOF: length %d, error %v", output.Len(), err)
			}
			if err := s.finish(); err != nil {
				t.Fatal(err)
			}
			for _, n := range output.sizes {
				if n <= 0 || n > outputBatchSize {
					t.Fatalf("invalid output batch: %d", n)
				}
			}
			if want := (size + outputBatchSize - 1) / outputBatchSize; len(output.sizes) != want {
				t.Fatalf("got %d batches, want %d", len(output.sizes), want)
			}
		})
	}
}

func TestFinishFlushesPendingOutput(t *testing.T) {
	e := testEngine(t)
	var output bytes.Buffer
	s, err := e.newStream(context.Background(), &output, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.write([]byte("<")); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("expected parser to retain unfinished tag")
	}
	if err := s.finish(); err != nil || output.String() != "<" {
		t.Fatalf("finish: %q, %v", &output, err)
	}
	failed, err := e.newStream(context.Background(), failWriter{}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer failed.close()
	if err := failed.write([]byte("<")); err != nil {
		t.Fatal(err)
	}
	if err := failed.finish(); !errors.Is(err, io.ErrClosedPipe) || !failed.module.IsClosed() {
		t.Fatalf("finish lost writer failure or left instance open: %v", err)
	}
}

func TestExpandedBatchOutput(t *testing.T) {
	e := testEngine(t)
	input := benchmarkInput("dense", 1024)
	want, _, err := rewriteMode(e, input, len(input), false)
	if err != nil {
		t.Fatal(err)
	}
	var output recordingWriter
	s, err := e.newStream(context.Background(), &output, len(want))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.write(input); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(); err != nil || !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("expanded output differs: %v", err)
	}
	if len(output.sizes) < 2 {
		t.Fatal("fixture did not span multiple output batches")
	}
	for _, n := range output.sizes {
		if n > outputBatchSize {
			t.Fatalf("expanded batch exceeded its bound: %d", n)
		}
	}
}

func TestOutputLimitAcrossBatches(t *testing.T) {
	e := testEngine(t)
	var output recordingWriter
	s, err := e.newStream(context.Background(), &output, outputBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err := s.write(bytes.Repeat([]byte("x"), 3*outputBatchSize)); err == nil || !s.module.IsClosed() {
		t.Fatalf("accepted excess batches: %v", err)
	}
	if err := s.finish(); err == nil || output.Len() != outputBatchSize || len(output.sizes) != 1 {
		t.Fatalf("failed stream emitted buffered suffix: length %d, writes %d, error %v", output.Len(), len(output.sizes), err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func TestBackpressureAndCancellation(t *testing.T) {
	e := testEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var signal, unblock sync.Once
	w := writerFunc(func(b []byte) (int, error) {
		signal.Do(func() { close(entered) })
		<-release
		return len(b), nil
	})
	s, err := e.newStream(ctx, w, 4096)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := s.write([]byte("<p>backpressure</p>"))
		if err == nil {
			err = s.finish()
		}
		done <- err
	}()
	defer func() {
		unblock.Do(func() { close(release) })
		// The receive below joins the writer before closing guest memory.
		if done != nil {
			<-done
		}
		_ = s.close()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer was never called")
	}
	select {
	case err := <-done:
		done = nil
		t.Fatalf("write bypassed blocked sink: %v", err)
	default:
	}
	cancel()
	unblock.Do(func() { close(release) })
	select {
	case err := <-done:
		done = nil
		if err == nil || !s.module.IsClosed() {
			t.Fatalf("canceled stream survived callback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled write did not finish after releasing its sink")
	}
}

func TestBatchingReducesCallbacks(t *testing.T) {
	e := testEngine(t)
	input := []byte(strings.Repeat(`<a class="rewrite">text</a>`, 1024))
	direct, directCalls, err := rewriteMode(e, input, 4096, false)
	if err != nil {
		t.Fatal(err)
	}
	batched, batchedCalls, err := rewriteMode(e, input, 4096, true)
	if err != nil || !bytes.Equal(direct, batched) {
		t.Fatalf("batching changed output: %v", err)
	}
	if batchedCalls >= directCalls/100 {
		t.Fatalf("batching failed to reduce callbacks: direct %d, batched %d", directCalls, batchedCalls)
	}
}
