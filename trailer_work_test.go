package statute

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

func TestHeaderNameSetMatchesEqualFold(t *testing.T) {
	for _, name := range []string{"X-Key", "X-Key", "X-ſuffix", "X-Σ", "X-ς", "invalid\xff"} {
		for _, other := range []string{"x-key", "X-KEY", "x-suffix", "X-σ", "X-ς", "invalid\xfe", "different"} {
			set := make(headerNameSet)
			set.add(name)
			if set.contains(other) != strings.EqualFold(name, other) {
				t.Fatalf("fold mismatch: %q / %q", name, other)
			}
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if next := unicode.SimpleFold(r); next != r && foldHeaderName(string(r)) != foldHeaderName(string(next)) {
			t.Fatalf("fold class mismatch: %U", r)
		}
	}
}

func trailerWorkHeaders() http.Header {
	return http.Header{
		"Trailer": {strings.Repeat("X-Missing, x-MISSING,", 1024) + "X-End, X-Preexisting"},
		"trailer": {" , x-end, "}, "X-End": {"one", "two"}, "x-end": {"alias"},
		"X-Ordinary": {"keep"}, http.TrailerPrefix + "X-Late": {"late"},
	}
}

func TestTrailerReplayDeduplicatesWithoutMutatingSource(t *testing.T) {
	b := newResponseBuffer()
	b.header = trailerWorkHeaders()
	before := b.header.Clone()
	_, _ = b.Write([]byte("body"))
	if got := len(responseTrailerNames(b.header)); got != 4 {
		t.Fatalf("trailer set contains %d names, want 4", got)
	}
	for range 2 {
		w := &trailerCommitWriter{ResponseRecorder: httptest.NewRecorder()}
		w.Header()["x-preexisting"] = []string{"destination"}
		b.replay(w)
		assertTrailerReplay(t, w)
	}
	if !reflect.DeepEqual(before, b.header) {
		t.Fatal("replay mutated source headers")
	}
}

func assertTrailerReplay(t *testing.T, w *trailerCommitWriter) {
	t.Helper()
	for key, want := range map[string][]string{"X-End": {"one", "two"}, "x-end": {"alias"}, "x-preexisting": {"destination"}} {
		if _, ok := w.committed[key]; ok {
			t.Fatalf("trailer %q committed as ordinary header", key)
		}
		if !reflect.DeepEqual(w.Header()[key], want) {
			t.Fatalf("replay lost trailer %q", key)
		}
	}
	if w.Body.String() != "body" || w.Header().Get("X-Ordinary") != "keep" {
		t.Fatal("replay lost ordinary response")
	}
	if values, ok := w.committed[http.TrailerPrefix+"X-Late"]; !ok || values != nil {
		t.Fatal("late trailer was not announced before commitment")
	}
}

type trailerCommitWriter struct {
	*httptest.ResponseRecorder
	committed http.Header
}

func (w *trailerCommitWriter) WriteHeader(code int) {
	w.committed = w.Header().Clone()
	w.ResponseRecorder.WriteHeader(code)
}

func TestTrailerStripRepeatedMissingNames(t *testing.T) {
	h := trailerWorkHeaders()
	stripResponseTrailers(h)
	if !reflect.DeepEqual(h, http.Header{"X-Ordinary": {"keep"}}) {
		t.Fatalf("unexpected retained metadata: %v", h)
	}
}

func TestCompressionDeduplicatesDeferredTrailerRemoval(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		rec := httptest.NewRecorder()
		rec.Header().Set("Trailer", strings.TrimSuffix(strings.Repeat("ETag, etag,", 1024), ","))
		rec.Header().Set("X-Ordinary", "keep")
		c := &compressResponseWriter{ResponseWriter: rec, encoded: true, coding: "gzip"}
		if rejected {
			c.droppedTrailers = stripUnacceptableRepresentation(rec.Header())
		} else {
			c.droppedTrailers = stripCompressionTrailers(rec.Header())
		}
		if len(c.droppedTrailers) != 1 {
			t.Fatalf("retained %d duplicate names", len(c.droppedTrailers))
		}
		rec.Header()["eTAG"] = []string{"late-origin"}
		if rejected {
			c.finishRejection()
		} else {
			c.finishTrailers()
		}
		if values, _ := cacheHeaderValues(rec.Header(), "ETag"); len(values) != 0 || rec.Header().Get("X-Ordinary") != "keep" {
			t.Fatal("deferred metadata removal failed")
		}
	}
}

func BenchmarkTrailerMetadataWork(b *testing.B) {
	for _, n := range []int{128, 512, 2048} {
		for _, repeated := range []bool{false, true} {
			h := make(http.Header)
			names := make([]string, n)
			for i := range n {
				h[fmt.Sprintf("X-Ordinary-%d", i)] = []string{"keep"}
				names[i] = fmt.Sprintf("X-Missing-%d", i)
				if repeated {
					names[i] = "ETag"
				}
			}
			h.Set("Trailer", strings.Join(names, ","))
			for _, operation := range []string{"replay", "strip", "compress", "reject"} {
				b.Run(fmt.Sprintf("%s/n=%d/repeated=%t", operation, n, repeated), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						cloned := h.Clone()
						benchmarkTrailerOperation(operation, cloned)
					}
				})
			}
		}
	}
}

func benchmarkTrailerOperation(operation string, h http.Header) {
	switch operation {
	case "replay":
		buf := newResponseBuffer()
		buf.header = h
		buf.replay(httptest.NewRecorder())
	case "strip":
		stripResponseTrailers(h)
	case "compress":
		stripCompressionTrailers(h)
	case "reject":
		stripUnacceptableRepresentation(h)
	}
}
