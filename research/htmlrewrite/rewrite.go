//go:build statute_htmlrewrite

package htmlrewrite

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"

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
	calls   int
	err     error
}

type engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

func newEngine(ctx context.Context) (*engine, error) {
	return newEngineConfigured(ctx, wazero.NewRuntimeConfigCompiler().
		WithMemoryLimitPages(512).WithCloseOnContextDone(true))
}

// Alternate configuration is used only for controlled research comparisons.
func newEngineConfigured(ctx context.Context, config wazero.RuntimeConfig) (*engine, error) {
	runtime := wazero.NewRuntimeWithConfig(ctx, config)
	host := runtime.NewHostModuleBuilder("sink")
	host.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, module api.Module, stack []uint64) {
			pointer, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
			sink := ctx.Value(sinkKey{}).(*outputSink)
			sink.calls++
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
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, nil).Export("emit")
	_, err := host.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(programExchange),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32}).Export("exchange").Instantiate(ctx)
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, fmt.Errorf("instantiate output sink: %w", err)
	}
	compiled, err := runtime.CompileModule(ctx, guest)
	if err != nil {
		_ = runtime.Close(context.Background())
		return nil, fmt.Errorf("compile rewriter: %w", err)
	}
	if err := validateGuest(compiled); err != nil {
		_ = runtime.Close(context.Background())
		return nil, err
	}
	return &engine{runtime: runtime, compiled: compiled}, nil
}

func (e *engine) close() error { return e.runtime.Close(context.Background()) }

// A stream has one caller; concurrent responses use independent instances.
type stream struct {
	module    api.Module
	functions map[string]api.Function
	ctx       context.Context
	sink      *outputSink
	input     uint32
	done      bool
}

func (e *engine) newStream(ctx context.Context, writer io.Writer, limit int) (*stream, error) {
	return e.newStreamMode(ctx, writer, limit, true)
}

func (e *engine) newStreamMode(ctx context.Context, writer io.Writer, limit int, buffered bool) (*stream, error) {
	return e.newConfiguredStream(ctx, writer, limit, buffered, nil)
}

func (e *engine) newConfiguredStream(ctx context.Context, writer io.Writer, limit int, buffered bool, program []byte) (*stream, error) {
	if writer == nil || limit < 0 {
		return nil, errors.New("invalid output sink")
	}
	sink := &outputSink{writer: writer, limit: limit}
	ctx = context.WithValue(ctx, sinkKey{}, sink)
	module, err := e.runtime.InstantiateModule(ctx, e.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, fmt.Errorf("instantiate rewriter: %w", err)
	}
	if err := checkGuestVersion(ctx, module); err != nil {
		_ = module.Close(context.Background())
		return nil, err
	}

	s := &stream{module: module, ctx: ctx, sink: sink, functions: make(map[string]api.Function, 3)}
	// Function handles carry call state, so reuse stays within one stream.
	for _, name := range []string{"create", "write", "finish"} {
		fn := module.ExportedFunction(name)
		if fn == nil {
			_ = s.close()
			return nil, fmt.Errorf("guest is missing %s", name)
		}
		s.functions[name] = fn
	}
	var mode uint64
	if buffered {
		mode = 1
	}
	pointer, err := module.ExportedFunction("input_pointer").Call(ctx)
	if err != nil {
		_ = s.close()
		return nil, err
	}
	s.input = uint32(pointer[0])
	if program != nil {
		if !module.Memory().Write(s.input, program) {
			_ = s.close()
			return nil, errors.New("program memory unavailable")
		}
		s.functions["create_program"] = module.ExportedFunction("create_program")
		if err := s.invoke("create_program", uint64(len(program)), mode); err != nil {
			return nil, err
		}
	} else if err := s.invoke("create", mode); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *stream) invoke(name string, args ...uint64) error {
	if s.done {
		return errors.New("rewriter is closed or finished")
	}
	fn := s.functions[name]
	if fn == nil {
		_ = s.close()
		return fmt.Errorf("unknown guest operation %q", name)
	}
	result, err := fn.Call(s.ctx, args...)
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

func validateGuest(compiled wazero.CompiledModule) error {
	if len(compiled.ExportedMemories()) != 1 || compiled.ExportedMemories()["memory"] == nil {
		return errors.New("guest memory export is incompatible")
	}
	if len(compiled.ImportedMemories()) != 0 || len(compiled.ImportedFunctions()) != 2 {
		return errors.New("guest capability set is incompatible")
	}
	expected := map[string]int{"emit": 2, "exchange": 4}
	seen := map[string]bool{}
	for _, fn := range compiled.ImportedFunctions() {
		module, name, ok := fn.Import()
		count, known := expected[name]
		if !ok || module != "sink" || !known || seen[name] || len(fn.ParamTypes()) != count {
			return errors.New("guest import is incompatible")
		}
		for _, param := range fn.ParamTypes() {
			if param != api.ValueTypeI32 {
				return errors.New("guest import parameter is incompatible")
			}
		}
		results := []api.ValueType(nil)
		if name == "exchange" {
			results = []api.ValueType{api.ValueTypeI32}
		}
		if !slices.Equal(fn.ResultTypes(), results) {
			return errors.New("guest import result is incompatible")
		}
		seen[name] = true
	}
	for name, count := range map[string]int{"program_abi": 0, "create": 1, "create_program": 2, "input_pointer": 0, "write": 1, "finish": 0} {
		fn := compiled.ExportedFunctions()[name]
		if fn == nil || len(fn.ParamTypes()) != count || !slices.Equal(fn.ResultTypes(), []api.ValueType{api.ValueTypeI32}) {
			return fmt.Errorf("guest export %s is incompatible", name)
		}
		for _, param := range fn.ParamTypes() {
			if param != api.ValueTypeI32 {
				return fmt.Errorf("guest export %s parameter is incompatible", name)
			}
		}
	}
	return nil
}

func checkGuestVersion(ctx context.Context, module api.Module) error {
	version, err := module.ExportedFunction("program_abi").Call(ctx)
	if err != nil {
		return fmt.Errorf("read guest ABI: %w", errors.Join(err, ctx.Err()))
	}
	if len(version) != 1 || version[0] != uint64(programABI) {
		return errors.New("guest protocol version is incompatible")
	}
	return nil
}
