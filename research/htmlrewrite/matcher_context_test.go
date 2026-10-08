//go:build statute_htmlrewrite

package htmlrewrite

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func matcherTrace(ctx context.Context, e *engine, input string, chunk int, buffered bool) ([]byte, error) {
	var output bytes.Buffer
	sink := &outputSink{writer: &output, limit: 1 << 20}
	ctx = context.WithValue(ctx, sinkKey{}, sink)
	module, err := e.runtime.InstantiateModule(ctx, e.compiled, wazero.NewModuleConfig().WithName(""))
	if err != nil {
		return nil, err
	}
	s := &stream{module: module, ctx: ctx, sink: sink, functions: map[string]api.Function{
		"create": module.ExportedFunction("create_matcher_probe"),
		"write":  module.ExportedFunction("write"),
		"finish": module.ExportedFunction("finish"),
	}}
	defer s.close()
	var mode uint64
	if buffered {
		mode = 1
	}
	if err := s.invoke("create", mode); err != nil {
		return nil, err
	}
	pointer, err := module.ExportedFunction("input_pointer").Call(ctx)
	if err != nil {
		return nil, err
	}
	s.input = uint32(pointer[0])
	for len(input) > 0 {
		n := min(chunk, len(input))
		if err := s.write([]byte(input[:n])); err != nil {
			return nil, err
		}
		input = input[n:]
	}
	if err := s.finish(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func TestMatcherContextNativeWasm(t *testing.T) {
	e := testEngine(t)
	cases := []struct {
		input   string
		retired string
	}{
		{`<div><span data-scope><span data-scope>x<!--in--></div>y<!--out-->`, "R 1 1\nR 2 0\nR 3 0\n"},
		{`<div><br><img src=x>x</div>`, "R 1 1\n"},
		{`<svg><path/><g>x</g></svg>`, "R 3 1\nR 1 1\n"},
		{`<div><span data-scope>x</aside></div><span data-scope>z</span>`, "R 1 1\nR 2 0\nR 3 1\n"},
		{`<div><span data-scope>é&amp;x`, ""},
		{`<ul><li>a<li>b</ul>`, "R 1 1\nR 2 0\nR 3 0\n"},
		{`<script>if (a < b) c()</script><style>a{color:red}</style>`, "R 1 1\nR 2 1\n"},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			for _, chunk := range []int{1, 7, 4096} {
				cmd := exec.CommandContext(ctx, "./guest/target/release/statute-htmlrewrite-spike", fmt.Sprint(chunk), "--matcher-probe")
				cmd.Stdin = strings.NewReader(tc.input)
				want, err := cmd.Output()
				if err != nil {
					t.Fatalf("native: %v", err)
				}
				var retired strings.Builder
				for line := range strings.SplitSeq(string(want), "\n") {
					if strings.HasPrefix(line, "R ") {
						retired.WriteString(line + "\n")
					}
				}
				if retired.String() != tc.retired {
					t.Fatalf("retirement: got %q, want %q", retired.String(), tc.retired)
				}
				for _, buffered := range []bool{false, true} {
					got, err := matcherTrace(ctx, e, tc.input, chunk, buffered)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("chunk=%d buffered=%v: %v\ngot %s\nwant %s", chunk, buffered, err, got, want)
					}
				}
			}
		})
	}
}

func TestMatcherContextConcurrentIsolation(t *testing.T) {
	e := testEngine(t)
	const input = `<div><span data-scope>x</div>`
	want, err := matcherTrace(t.Context(), e, input, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := matcherTrace(t.Context(), e, input, 1, true)
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("instance identities leaked: %v, %q", err, got)
			}
		})
	}
	wg.Wait()
}
