package statute

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestHostsToolingMatchesManualExpansion(t *testing.T) {
	t.Parallel()
	multi := Config{
		Listeners: Listeners{HTTP(":0")},
		Routes: Routes{
			Match("/first").Handle(noContentHandler),
			Match("/*").Hosts("a.example", "b.example").Handle(noContentHandler).With(RateLimit("1/h")),
			Match("/last").Handle(noContentHandler),
		},
	}
	manual := multi
	manual.Routes = Routes{multi.Routes[0],
		Match("/*").Host("a.example").Handle(noContentHandler).With(RateLimit("1/h")),
		Match("/*").Host("b.example").Handle(noContentHandler).With(RateLimit("1/h")),
		multi.Routes[2],
	}
	t.Run("export", func(t *testing.T) {
		var got, want bytes.Buffer
		if err := Export(multi, &got); err != nil {
			t.Fatal(err)
		}
		if err := Export(manual, &want); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatal("export differs from manual single-host expansion")
		}
	})
	t.Run("graph", func(t *testing.T) {
		var got, want bytes.Buffer
		if err := GraphDOT(multi, &got); err != nil {
			t.Fatal(err)
		}
		if err := GraphDOT(manual, &want); err != nil {
			t.Fatal(err)
		}
		if got.String() != want.String() || !strings.Contains(got.String(), "a.example") || !strings.Contains(got.String(), "b.example") {
			t.Fatal("graph does not match concrete single-host routes")
		}
	})
	t.Run("lint", func(t *testing.T) { assertHostsLintExpansion(t, multi, manual) })
}

func assertHostsLintExpansion(t *testing.T, multi, manual Config) {
	t.Helper()
	got, err := Lint(multi)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Lint(manual)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lint differs: got=%v want=%v", got, want)
	}
	var paths []string
	for _, finding := range got {
		if finding.Code == "RL001" {
			paths = append(paths, finding.Path)
		}
	}
	if !slices.Equal(paths, []string{"routes[1].middleware[0]", "routes[2].middleware[0]"}) {
		t.Fatalf("lint paths=%v; want concrete expanded indexes", paths)
	}
}
