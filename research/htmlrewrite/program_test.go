//go:build statute_htmlrewrite

package htmlrewrite

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testSession struct {
	onHandle func(context.Context, eventSnapshot, *mutations) error
	onClose  func() error
}

func (s *testSession) handle(ctx context.Context, e eventSnapshot, m *mutations) error {
	if s.onHandle != nil {
		return s.onHandle(ctx, e, m)
	}
	return nil
}

func (s *testSession) close() error {
	if s.onClose != nil {
		return s.onClose()
	}
	return nil
}

func testProgramLimits() programLimits {
	return programLimits{programBytes: 65536, eventBytes: 65536, commandBytes: 65536, commands: 64, scopes: 128, events: 8192, inputBytes: 1 << 20, outputBytes: 1 << 20, parserBytes: 1 << 20}
}

func testProgram(t testing.TB, p programDefinition) *preparedProgram {
	t.Helper()
	v, err := prepareProgram(p, testProgramLimits(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func testRun(t testing.TB, capacity int) *programRun {
	t.Helper()
	r, err := newProgramRun(context.Background(), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return r
}

func feedProgram(s *programStream, input string, chunk int) error {
	for len(input) > 0 {
		n := min(chunk, len(input))
		if err := s.write([]byte(input[:n])); err != nil {
			return err
		}
		input = input[n:]
	}
	return s.finish()
}

func nativeProgram(t *testing.T, input string, chunk int, wire []byte, replies [][]byte, events ...[][]byte) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	programPath := filepath.Join(dir, "program")
	replyPath := filepath.Join(dir, "replies")
	eventPath := filepath.Join(dir, "events")
	if err := os.WriteFile(programPath, wire, 0o600); err != nil {
		t.Fatal(err)
	}
	var frames []byte
	for _, reply := range replies {
		frames = binary.LittleEndian.AppendUint32(frames, uint32(len(reply)))
		frames = append(frames, reply...)
	}
	if err := os.WriteFile(replyPath, frames, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("guest/target/release/statute-htmlrewrite-spike", fmt.Sprint(chunk), "--program", programPath, replyPath, eventPath)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err == nil && len(events) > 0 {
		actual, readErr := os.ReadFile(eventPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var expected []byte
		for _, event := range events[0] {
			expected = binary.LittleEndian.AppendUint32(expected, uint32(len(event)))
			expected = append(expected, event...)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("native/Wasm event snapshots differ (native %d, Wasm %d bytes)", len(actual), len(expected))
		}
	}
	return out, err
}

func TestProgramDeclarativeNativeWasm(t *testing.T) {
	p := testProgram(t, programDefinition{rules: []programRule{
		{selector: "a", event: elementEvent, commands: []command{{op: setAttribute, name: "href", content: "/x?a=1&b=2"}, {op: appendContent, content: "<safe>"}}},
		{selector: ".remove", event: elementEvent, commands: []command{{op: removeContent}}},
		{event: documentEndEvent, commands: []command{{op: appendContent, html: true, content: "<!--done-->"}}},
	}})
	input := `<a href="old">hello</a><div class="remove">secret</div>`
	want := `<a href="/x?a=1&b=2">hello&lt;safe&gt;</a><!--done-->`
	for _, chunk := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			r := testRun(t, 1)
			var out bytes.Buffer
			s, err := r.start(context.Background(), p, requestMetadata{}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if err := feedProgram(s, input, chunk); err != nil {
				t.Fatal(err)
			}
			native, err := nativeProgram(t, input, chunk, p.wire, nil)
			if err != nil {
				t.Fatalf("native: %s: %v", native, err)
			}
			if out.String() != want || string(native) != want {
				t.Fatalf("wasm=%q native=%q want=%q", out.String(), native, want)
			}
		})
	}
}

func TestProgramCallbackSnapshotsAndParity(t *testing.T) {
	for _, chunk := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			r := testRun(t, 1)
			var events []eventSnapshot
			var builders []*mutations
			closes := 0
			p := testProgram(t, programDefinition{rules: []programRule{
				{selector: "span", event: elementEvent, callback: true},
				{selector: "span[data-scope]", event: elementEvent, callback: true},
				{event: documentEndEvent, callback: true},
				{selector: "span", event: textEvent, callback: true},
				{selector: "span[data-scope]", event: textEvent, callback: true},
				{selector: "span", event: commentEvent, callback: true},
				{selector: "span", event: endTagEvent, callback: true},
			}, newSession: func(_ context.Context, m requestMetadata) (callbackSession, error) {
				if m.headers["X-Test"][0] != "original" {
					t.Fatal("wrong metadata")
				}
				m.headers["X-Test"][0] = "private"
				return &testSession{onHandle: func(ctx context.Context, e eventSnapshot, m *mutations) error {
					events = append(events, e)
					builders = append(builders, m)
					switch e.id.kind {
					case elementEvent:
						if e.id.rule == 0 {
							return m.add(ctx, command{op: setAttribute, name: "seen", content: "yes"})
						}
						if !reflect.DeepEqual(e.attributes, []attributeSnapshot{{"data-scope", ""}, {"title", "a&amp;b"}, {"seen", "yes"}}) {
							return fmt.Errorf("later handler did not see mutations: %v", e.attributes)
						}
					case documentEndEvent:
						return m.add(ctx, command{op: appendContent, html: true, content: "<!--end-->"})
					}
					return nil
				}, onClose: func() error { closes++; return nil }}, nil
			}})
			metadata := requestMetadata{headers: map[string][]string{"X-Test": {"original"}}}
			var out bytes.Buffer
			s, err := r.start(context.Background(), p, metadata, &out)
			if err != nil {
				t.Fatal(err)
			}
			s.child.capture = true
			input := `<div><span data-scope title="a&amp;b"><span data-scope title="a&amp;b">x&amp;y<!--in--></div>outside<span>z</span><script>script</script>`
			if err := feedProgram(s, input, chunk); err != nil {
				t.Fatal(err)
			}
			if metadata.headers["X-Test"][0] != "original" || closes != 1 || r.active() != 0 {
				t.Fatal("metadata or session ownership escaped")
			}
			native, err := nativeProgram(t, input, chunk, p.wire, s.child.replies, s.child.events)
			if err != nil {
				t.Fatalf("native: %s: %v", native, err)
			}
			if !bytes.Equal(native, out.Bytes()) {
				t.Fatalf("native/wasm differ: %q / %q", native, out.String())
			}
			seenNested := false
			seenEnd := 0
			seenDocument := 0
			for _, e := range events {
				if e.id.kind == textEvent && len(e.scopes) == 2 {
					seenNested = true
				}
				if e.id.kind == endTagEvent {
					seenEnd++
					if e.name != "span" {
						t.Fatal("implicit descendant acquired ancestor end token")
					}
				}
				if e.id.kind == documentEndEvent {
					seenDocument++
				}
			}
			if !seenNested || seenEnd != 1 || seenDocument != 1 {
				t.Fatalf("missing scope/end/EOF: nested=%v end=%d EOF=%d", seenNested, seenEnd, seenDocument)
			}
			for _, builder := range builders {
				if err := builder.add(context.Background(), command{op: removeContent}); err == nil {
					t.Fatal("retained builder accepted mutation")
				}
			}
			if events[0].attributes[1].entityValue != "a&amp;b" {
				t.Fatal("snapshot lost entity representation")
			}
			if len(s.child.replies) > 1 {
				foreign := append([]byte(nil), s.child.replies[0]...)
				binary.LittleEndian.PutUint64(foreign[4:12], 999)
				replies := append([][]byte(nil), s.child.replies...)
				replies[0] = foreign
				if got, err := nativeProgram(t, input, chunk, p.wire, replies); err == nil {
					t.Fatalf("native accepted foreign reply: %s", got)
				}
			}
		})
	}
}

func TestProgramTerminalFailures(t *testing.T) {
	for _, failure := range []string{"factory-error", "factory-panic", "handle-error", "handle-panic", "invalid-command", "close-error", "close-panic", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			r, err := newProgramRun(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			closes := 0
			p := testProgram(t, programDefinition{rules: []programRule{{selector: "div", event: elementEvent, callback: true}}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
				switch failure {
				case "factory-error":
					return nil, errors.New("factory failure")
				case "factory-panic":
					panic("factory")
				}
				return &testSession{onHandle: func(ctx context.Context, _ eventSnapshot, m *mutations) error {
					switch failure {
					case "handle-error":
						return errors.New("handle failure")
					case "handle-panic":
						panic("handle")
					case "invalid-command":
						_ = m.add(ctx, command{op: operation(999)})
					case "cancel":
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}, onClose: func() error {
					closes++
					switch failure {
					case "close-error":
						return errors.New("cleanup failure")
					case "close-panic":
						panic("close")
					}
					return nil
				}}, nil
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := r.start(ctx, p, requestMetadata{}, io.Discard)
			if failure == "cancel" {
				cancel()
			}
			if err == nil {
				err = feedProgram(s, "<div>payload</div>", 7)
				_ = s.close()
			}
			if err == nil {
				t.Fatal("terminal failure was accepted")
			}
			if r.active() != 0 {
				t.Fatal("failed child retained admission")
			}
			if strings.HasPrefix(failure, "factory-") {
				if closes != 0 {
					t.Fatal("unreturned session closed")
				}
			} else if closes != 1 {
				t.Fatalf("close count %d", closes)
			}
			shutdownErr := r.shutdown(context.Background())
			if strings.HasPrefix(failure, "close-") && shutdownErr == nil {
				t.Fatal("cleanup failure was lost")
			}
			if r.finalized.Load() != 1 {
				t.Fatal("runtime finalization count")
			}
		})
	}
}

func TestProgramImmutableAndConcurrentIsolation(t *testing.T) {
	rules := []programRule{{selector: "a", event: elementEvent, commands: []command{{op: setAttribute, name: "x", content: "frozen"}}}}
	p := testProgram(t, programDefinition{rules: rules})
	rules[0].selector = "invalid["
	rules[0].commands[0].content = "changed"
	r := testRun(t, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			var out bytes.Buffer
			s, err := r.start(context.Background(), p, requestMetadata{}, &out)
			if err == nil {
				err = feedProgram(s, "<a></a>", 1)
			}
			if err != nil || out.String() != `<a x="frozen"></a>` {
				t.Errorf("isolated program: %q %v", out.String(), err)
			}
		})
	}
	wg.Wait()
}

func TestProgramUnfinishedOwnership(t *testing.T) {
	for _, phase := range []string{"factory", "handle", "close"} {
		t.Run(phase, func(t *testing.T) {
			r, err := newProgramRun(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var closes atomic.Int32
			block := func() { close(entered); <-release }
			p := testProgram(t, programDefinition{rules: []programRule{{selector: "div", event: elementEvent, callback: true}}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
				if phase == "factory" {
					block()
				}
				return &testSession{onHandle: func(context.Context, eventSnapshot, *mutations) error {
					if phase == "handle" {
						block()
					}
					return nil
				}, onClose: func() error {
					closes.Add(1)
					if phase == "close" {
						block()
					}
					return nil
				}}, nil
			}})
			go func() {
				s, e := r.start(context.Background(), p, requestMetadata{}, io.Discard)
				if e == nil {
					e = feedProgram(s, "<div></div>", 4096)
				}
				done <- e
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("callback never entered")
			}
			for range 2 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				err := r.shutdown(ctx)
				cancel()
				if err == nil || r.active() != 1 || r.finalized.Load() != 0 {
					t.Fatal("shutdown falsely released blocked work")
				}
			}
			if _, err := r.start(context.Background(), p, requestMetadata{}, io.Discard); err == nil {
				t.Fatal("retired run admitted work")
			}
			close(release)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("released callback did not retire")
			}
			if err := r.shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if closes.Load() != 1 || r.active() != 0 || r.finalized.Load() != 1 {
				t.Fatal("eventual cleanup ownership")
			}
		})
	}
}

func TestProgramKillableUncooperativeCallbacks(t *testing.T) {
	if phase := os.Getenv("STATUTE_PROGRAM_STALL"); phase != "" {
		r, err := newProgramRun(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		block := func() { close(entered); select {} }
		p := testProgram(t, programDefinition{rules: []programRule{{selector: "a", event: elementEvent, callback: true}}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
			if phase == "factory" {
				block()
			}
			return &testSession{onHandle: func(context.Context, eventSnapshot, *mutations) error {
				if phase == "handle" {
					block()
				}
				return nil
			}, onClose: func() error {
				if phase == "close" {
					block()
				}
				return nil
			}}, nil
		}})
		go func() {
			s, err := r.start(context.Background(), p, requestMetadata{}, io.Discard)
			if err == nil {
				_ = feedProgram(s, "<a></a>", 4096)
			}
		}()
		<-entered
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if r.shutdown(ctx) == nil || r.active() != 1 || r.finalized.Load() != 0 {
			t.Fatal("unfinished callback falsely retired")
		}
		fmt.Println("retained")
		time.Sleep(time.Hour)
		return
	}
	for _, phase := range []string{"factory", "handle", "close"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProgramKillableUncooperativeCallbacks$")
			cmd.Env = append(os.Environ(), "STATUTE_PROGRAM_STALL="+phase)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			line, readErr := bufio.NewReader(stdout).ReadString('\n')
			if line != "retained\n" || readErr != nil {
				cancel()
				_ = cmd.Wait()
				t.Fatalf("child ownership proof: %q %v %s", line, readErr, stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("uncooperative subprocess exited without kill")
			}
		})
	}
}

func TestProgramBoundsInitializationAndReentry(t *testing.T) {
	for _, failure := range []string{"selector", "input", "output", "event", "commands", "scopes", "events", "reentry"} {
		t.Run(failure, func(t *testing.T) {
			r := testRun(t, 1)
			limits := testProgramLimits()
			closes := 0
			rule := programRule{selector: "div", event: elementEvent, callback: true}
			switch failure {
			case "selector":
				rule.selector = "["
			case "input":
				limits.inputBytes = 2
			case "output":
				limits.outputBytes = 2
			case "event":
				limits.eventBytes = 80
			case "commands":
				limits.commands = 1
			case "scopes":
				limits.scopes = 1
			case "events":
				limits.events = 1
			}
			var stream *programStream
			definition := programDefinition{rules: []programRule{rule}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
				return &testSession{onHandle: func(ctx context.Context, _ eventSnapshot, m *mutations) error {
					if failure == "commands" {
						_ = m.add(ctx, command{op: setAttribute, name: "x", content: "1"})
						_ = m.add(ctx, command{op: setAttribute, name: "y", content: "2"})
					}
					if failure == "reentry" {
						_ = stream.write([]byte("nested"))
					}
					return nil
				}, onClose: func() error { closes++; return nil }}, nil
			}}
			p, err := prepareProgram(definition, limits, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			stream, err = r.start(context.Background(), p, requestMetadata{}, io.Discard)
			if err == nil {
				err = feedProgram(stream, "<div><div>content</div></div>", 4096)
				_ = stream.close()
			}
			if err == nil || closes != 1 || r.active() != 0 {
				t.Fatalf("failure %s: err=%v closes=%d active=%d", failure, err, closes, r.active())
			}
		})
	}
}

func TestProgramRejectsMalformedWire(t *testing.T) {
	e := testEngine(t)
	p := testProgram(t, programDefinition{rules: []programRule{{selector: "a", event: elementEvent, commands: []command{{op: removeContent}}}}})
	for _, failure := range []string{"version", "truncated", "count", "opcode", "boolean", "suffix"} {
		t.Run(failure, func(t *testing.T) {
			wire := append([]byte(nil), p.wire...)
			switch failure {
			case "version":
				binary.LittleEndian.PutUint32(wire, 999)
			case "truncated":
				wire = wire[:len(wire)-1]
			case "count":
				binary.LittleEndian.PutUint32(wire[36:], ^uint32(0))
			case "opcode":
				binary.LittleEndian.PutUint32(wire[57:], 999)
			case "boolean":
				binary.LittleEndian.PutUint32(wire[61:], 2)
			case "suffix":
				wire = append(wire, 0)
			}
			if s, err := e.newConfiguredStream(context.Background(), io.Discard, 4096, true, wire); err == nil {
				_ = s.close()
				t.Fatal("malformed program accepted")
			}
		})
	}
}

func TestProgramTextUnitsAndContexts(t *testing.T) {
	r := testRun(t, 1)
	var events []eventSnapshot
	p := testProgram(t, programDefinition{rules: []programRule{{selector: "*", event: textEvent, callback: true}}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
		return &testSession{onHandle: func(ctx context.Context, e eventSnapshot, _ *mutations) error { events = append(events, e); return nil }}, nil
	}})
	var out bytes.Buffer
	s, err := r.start(context.Background(), p, requestMetadata{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	s.child.capture = true
	input := `<div>x&amp;y</div><script>a<b && c</script><style>a>b</style>`
	if err := feedProgram(s, input, 1); err != nil {
		t.Fatal(err)
	}
	if out.String() != input {
		t.Fatal("text bytes changed")
	}
	var joined strings.Builder
	prior := uint64(0)
	var chunk uint32
	contexts := map[string]bool{}
	emptyLast := false
	for _, e := range events {
		contexts[e.context] = true
		joined.WriteString(e.text)
		if e.textUnit != prior {
			if e.chunk != 0 {
				t.Fatal("new text unit has nonzero chunk")
			}
			prior = e.textUnit
			chunk = 0
		} else {
			chunk++
			if e.chunk != chunk {
				t.Fatal("text chunk sequence changed")
			}
		}
		if e.last && e.text == "" {
			emptyLast = true
		}
	}
	if !strings.Contains(joined.String(), "x&amp;y") || !contexts["ScriptData"] || !contexts["RawText"] || !emptyLast {
		t.Fatalf("text contract: %q contexts=%v emptyLast=%v", joined.String(), contexts, emptyLast)
	}
	if _, err := nativeProgram(t, input, 1, p.wire, s.child.replies, s.child.events); err != nil {
		t.Fatal(err)
	}
}

func TestProgramMutationsAcrossEventKinds(t *testing.T) {
	cases := []struct {
		name, input, selector string
		kind                  eventKind
		commands              []command
		want                  string
	}{
		{"attribute-remove", `<a x="y"></a>`, "a", elementEvent, []command{{op: removeAttribute, name: "x"}}, `<a></a>`},
		{"element-wrap", `<a>old</a>`, "a", elementEvent, []command{{op: beforeContent, content: "<"}, {op: prependContent, content: "first"}, {op: appendContent, html: true, content: "<b>last</b>"}, {op: afterContent, content: ">"}}, `&lt;<a>firstold<b>last</b></a>&gt;`},
		{"inner", `<a>old</a>`, "a", elementEvent, []command{{op: innerContent, content: "<&"}}, `<a>&lt;&amp;</a>`},
		{"element-replace", `<a>old</a>`, "a", elementEvent, []command{{op: replaceContent, html: true, content: "<b>new</b>"}}, `<b>new</b>`},
		{"comment", `<a><!--old--></a>`, "a", commentEvent, []command{{op: replaceContent, content: "<new>"}}, `<a>&lt;new&gt;</a>`},
		{"end", `<a>old</a>`, "a", endTagEvent, []command{{op: beforeContent, content: "!"}, {op: afterContent, content: "?"}}, `<a>old!</a>?`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRun(t, 1)
			p := testProgram(t, programDefinition{rules: []programRule{{selector: tc.selector, event: tc.kind, commands: tc.commands}}})
			var out bytes.Buffer
			s, err := r.start(context.Background(), p, requestMetadata{}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if err := feedProgram(s, tc.input, 7); err != nil {
				t.Fatal(err)
			}
			native, err := nativeProgram(t, tc.input, 7, p.wire, nil)
			if err != nil || string(native) != tc.want || out.String() != tc.want {
				t.Fatalf("native=%q wasm=%q want=%q err=%v", native, out.String(), tc.want, err)
			}
		})
	}
}

func TestProgramForeignBuilderCannotMutateSibling(t *testing.T) {
	r := testRun(t, 2)
	builders := make(chan *mutations, 1)
	release := make(chan struct{})
	done := make(chan error, 1)
	p := testProgram(t, programDefinition{rules: []programRule{{selector: "a", event: elementEvent, callback: true}}, newSession: func(_ context.Context, metadata requestMetadata) (callbackSession, error) {
		return &testSession{onHandle: func(ctx context.Context, _ eventSnapshot, m *mutations) error {
			if metadata.url == "first" {
				builders <- m
				<-release
				return nil
			}
			foreign := <-builders
			_ = foreign.add(ctx, command{op: setAttribute, name: "leak", content: "foreign"})
			return nil
		}}, nil
	}})
	var first bytes.Buffer
	a, err := r.start(context.Background(), p, requestMetadata{url: "first"}, &first)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- feedProgram(a, "<a></a>", 4096) }()
	b, err := r.start(context.Background(), p, requestMetadata{url: "second"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := feedProgram(b, "<a></a>", 4096); err == nil {
		t.Error("foreign builder was accepted")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("foreign attempt failed its sibling: %v", err)
	}
	if first.String() != `<a></a>` {
		t.Fatalf("foreign mutation escaped: %q", first.String())
	}
	if r.active() != 0 {
		t.Fatal("foreign builder attempt retained admission")
	}
}

func TestProgramRetainedBuilderFailsCurrentInvocation(t *testing.T) {
	r := testRun(t, 1)
	var retained *mutations
	closes := 0
	documentEnd := false
	p := testProgram(t, programDefinition{rules: []programRule{{selector: "a", event: elementEvent, callback: true}, {event: documentEndEvent, callback: true}}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
		return &testSession{onHandle: func(ctx context.Context, e eventSnapshot, m *mutations) error {
			if e.id.kind == documentEndEvent {
				documentEnd = true
				return nil
			}
			if retained == nil {
				retained = m
			} else {
				_ = retained.add(ctx, command{op: removeContent})
			}
			return nil
		}, onClose: func() error { closes++; return nil }}, nil
	}})
	s, err := r.start(context.Background(), p, requestMetadata{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := feedProgram(s, "<a></a><a></a>", 4096); err == nil {
		t.Fatal("ignored stale builder did not fail current invocation")
	}
	if closes != 1 || documentEnd || r.active() != 0 {
		t.Fatal("aborted session violated EOF or cleanup ownership")
	}
}

func TestProgramCancelledAdmissionAndSiblingRecovery(t *testing.T) {
	r := testRun(t, 1)
	factories := 0
	closes := 0
	p := testProgram(t, programDefinition{rules: []programRule{{selector: "a", event: elementEvent, callback: true}}, newSession: func(context.Context, requestMetadata) (callbackSession, error) {
		factories++
		return &testSession{onClose: func() error { closes++; return nil }}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.start(ctx, p, requestMetadata{}, io.Discard); err == nil || factories != 0 || r.active() != 0 {
		t.Fatal("cancelled admission entered application code")
	}
	a, err := r.start(context.Background(), p, requestMetadata{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.start(context.Background(), p, requestMetadata{}, io.Discard); err == nil || factories != 1 {
		t.Fatal("exhausted admission entered factory")
	}
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	b, err := r.start(context.Background(), p, requestMetadata{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := feedProgram(b, "<a></a>", 1); err != nil {
		t.Fatal(err)
	}
	if factories != 2 || closes != 2 || r.active() != 0 {
		t.Fatal("admission did not recover with independent session")
	}
}

func TestProgramRejectsMissingGuestCapabilities(t *testing.T) {
	e := testEngine(t)
	empty, err := e.runtime.CompileModule(context.Background(), []byte{0, 97, 115, 109, 1, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close(context.Background())
	if err := validateGuest(empty); err == nil {
		t.Fatal("guest without the memory and callback contract was accepted")
	}
}

func TestProgramRejectsAmbiguousRulesAndCommands(t *testing.T) {
	for _, rule := range []programRule{
		{selector: "a", event: documentEndEvent},
		{selector: "a", event: elementEvent, commands: []command{{op: removeAttribute, name: "bad name"}}},
		{selector: "a", event: elementEvent, commands: []command{{op: removeContent, content: "ignored"}}},
		{selector: "a", event: elementEvent, commands: []command{{op: beforeContent, name: "ignored"}}},
		{selector: "a", event: elementEvent, callback: true, commands: []command{{op: removeContent}}},
	} {
		_, err := prepareProgram(programDefinition{rules: []programRule{rule}, newSession: func(context.Context, requestMetadata) (callbackSession, error) { return &testSession{}, nil }}, testProgramLimits(), time.Second)
		if err == nil {
			t.Fatalf("ambiguous rule accepted: %+v", rule)
		}
	}
}

func TestProgramABICheckReportsCancellation(t *testing.T) {
	e := testEngine(t)
	s, err := e.newStream(context.Background(), io.Discard, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = checkGuestVersion(ctx, s.module)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ABI check lost cancellation: %v", err)
	}
	if strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("cancellation reported as an ABI mismatch: %v", err)
	}
}

func TestProgramDocumentEndRegistrationOrder(t *testing.T) {
	for _, chunk := range []int{1, 7, 4096} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			r := testRun(t, 1)
			var order []uint32
			p := testProgram(t, programDefinition{
				rules: []programRule{
					{event: documentEndEvent, commands: []command{{op: appendContent, content: "A"}}},
					{event: documentEndEvent, callback: true},
					{event: documentEndEvent, commands: []command{{op: appendContent, content: "C"}}},
					{event: documentEndEvent, callback: true},
				},
				newSession: func(context.Context, requestMetadata) (callbackSession, error) {
					return &testSession{onHandle: func(ctx context.Context, e eventSnapshot, m *mutations) error {
						order = append(order, e.id.rule)
						return m.add(ctx, command{op: appendContent, content: fmt.Sprint(len(order))})
					}}, nil
				},
			})
			var out bytes.Buffer
			s, err := r.start(context.Background(), p, requestMetadata{}, &out)
			if err != nil {
				t.Fatal(err)
			}
			s.child.capture = true
			if err := feedProgram(s, "<p>x</p>", chunk); err != nil {
				t.Fatal(err)
			}
			if out.String() != "<p>x</p>A1C2" || !reflect.DeepEqual(order, []uint32{1, 3}) {
				t.Errorf("EOF output=%q callbacks=%v; want <p>x</p>A1C2 and [1 3]", out.String(), order)
			}
			native, err := nativeProgram(t, "<p>x</p>", chunk, p.wire, s.child.replies, s.child.events)
			if err != nil || !bytes.Equal(native, out.Bytes()) {
				t.Fatalf("native=%q wasm=%q err=%v", native, out.String(), err)
			}
		})
	}
}

func TestProgramForeignBuilderAcrossRuns(t *testing.T) {
	aRun, bRun := testRun(t, 1), testRun(t, 1)
	type offer struct {
		builder *mutations
		id      invocationIdentity
	}
	builders := make(chan offer, 1)
	release, done := make(chan struct{}), make(chan error, 1)
	var aID, bID invocationIdentity
	p := testProgram(t, programDefinition{
		rules: []programRule{{selector: "a", event: elementEvent, callback: true}},
		newSession: func(_ context.Context, metadata requestMetadata) (callbackSession, error) {
			return &testSession{onHandle: func(ctx context.Context, e eventSnapshot, m *mutations) error {
				if metadata.url == "first" {
					builders <- offer{m, e.id}
					<-release
					return nil
				}
				foreign := <-builders
				aID, bID = foreign.id, e.id
				_ = foreign.builder.add(ctx, command{op: setAttribute, name: "leak", content: "foreign"})
				return nil
			}}, nil
		},
	})
	var first bytes.Buffer
	a, err := aRun.start(context.Background(), p, requestMetadata{url: "first"}, &first)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- feedProgram(a, "<a></a>", 4096) }()
	b, err := bRun.start(context.Background(), p, requestMetadata{url: "second"}, io.Discard)
	if err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	if err := feedProgram(b, "<a></a>", 4096); err == nil {
		t.Error("builder accepted another run's context")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("foreign attempt failed the first run: %v", err)
	}
	if aID != bID {
		t.Fatalf("test did not collide wire identities: %v != %v", aID, bID)
	}
	if first.String() != `<a></a>` {
		t.Errorf("foreign mutation escaped: %q", first.String())
	}
	if aRun.active() != 0 || bRun.active() != 0 {
		t.Fatal("run did not release its stream")
	}
}
