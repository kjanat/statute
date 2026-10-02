// Package htmlrewrite contains an isolated HTML-rewriting feasibility experiment.
package htmlrewrite

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Generated explicitly by make build; production Go builds never run Rust.
//
//go:embed artifact/rewriter.wasm
var guest []byte

const chunkSize = 65536

type sinkKey struct{}

type outputSink struct {
	writer  io.Writer
	limit   int
	written int
	err     error
}

type engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

func newEngine(ctx context.Context) (*engine, error) {
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler().
		WithMemoryLimitPages(512).WithCloseOnContextDone(true))
	_, err := runtime.NewHostModuleBuilder("sink").NewFunctionBuilder().
		WithFunc(func(ctx context.Context, module api.Module, pointer, length uint32) {
			sink := ctx.Value(sinkKey{}).(*outputSink)
			bytes, ok := module.Memory().Read(pointer, length)
			if !ok {
				sink.err = errors.New("guest emitted an invalid memory range")
			} else if uint64(length) > uint64(sink.limit-sink.written) {
				sink.err = errors.New("rewrite output limit exceeded")
			} else {
				// Copy before calling a writer that might retain guest memory.
				n, writeErr := sink.writer.Write(append([]byte(nil), bytes...))
				sink.written += n
				sink.err = writeErr
				if writeErr == nil && n != len(bytes) {
					sink.err = io.ErrShortWrite
				}
			}
			if sink.err != nil {
				// The Rust sink is infallible; wazero turns this panic into a call error.
				panic(sink.err)
			}
		}).Export("emit").Instantiate(ctx)
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, fmt.Errorf("instantiate output sink: %w", err)
	}
	compiled, err := runtime.CompileModule(ctx, guest)
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, fmt.Errorf("compile rewriter: %w", err)
	}
	return &engine{runtime: runtime, compiled: compiled}, nil
}

func (e *engine) close() error { return e.runtime.Close(context.Background()) }

// A stream has one caller; concurrent responses use independent instances.
type stream struct {
	module api.Module
	ctx    context.Context
	sink   *outputSink
	input  uint32
	done   bool
}

func (e *engine) newStream(ctx context.Context, writer io.Writer, limit int) (*stream, error) {
	if writer == nil || limit < 0 {
		return nil, errors.New("invalid output sink")
	}
	sink := &outputSink{writer: writer, limit: limit}
	ctx = context.WithValue(ctx, sinkKey{}, sink)
	module, err := e.runtime.InstantiateModule(ctx, e.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiate rewriter: %w", err)
	}
	s := &stream{module: module, ctx: ctx, sink: sink}
	if err := s.invoke("create"); err != nil {
		return nil, err
	}
	pointer, err := module.ExportedFunction("input_pointer").Call(ctx)
	if err != nil {
		_ = s.close()
		return nil, err
	}
	s.input = uint32(pointer[0])
	return s, nil
}

func (s *stream) invoke(name string, args ...uint64) error {
	if s.done {
		return errors.New("rewriter is closed or finished")
	}
	result, err := s.module.ExportedFunction(name).Call(s.ctx, args...)
	if err == nil && (len(result) != 1 || result[0] != 0) {
		err = fmt.Errorf("guest %s failed: %v", name, result)
	}
	if err != nil {
		_ = s.close()
		return fmt.Errorf("rewrite %s: %w", name, errors.Join(err, s.sink.err))
	}
	return nil
}

func (s *stream) write(bytes []byte) error {
	if s.done {
		return errors.New("rewriter is closed or finished")
	}
	for len(bytes) > 0 {
		n := min(chunkSize, len(bytes))
		if !s.module.Memory().Write(s.input, bytes[:n]) {
			_ = s.close()
			return errors.New("guest input region is unavailable")
		}
		if err := s.invoke("write", uint64(n)); err != nil {
			return err
		}
		bytes = bytes[n:]
	}
	return nil
}

func (s *stream) finish() error {
	err := s.invoke("finish")
	_ = s.close()
	return err
}

func (s *stream) close() error {
	s.done = true
	return s.module.Close(context.Background())
}
