package httpprecondition

import (
	"net/http"
	"reflect"
	"testing"
)

func TestStatus(t *testing.T) {
	selected := http.Header{"ETag": {`"current"`}, "Last-Modified": {"Wed, 21 Oct 2015 07:28:00 GMT"}}
	for _, tc := range []struct {
		name string
		h    http.Header
		want int
	}{
		{"ordinary", nil, 0},
		{"if match", http.Header{"If-Match": {`"current"`}}, 0},
		{"if match star", http.Header{"If-Match": {"*"}}, 0},
		{"if match list", http.Header{"If-Match": {`"other", "current"`}}, 0},
		{"if match weak", http.Header{"If-Match": {`W/"current"`}}, 412},
		{"if match different", http.Header{"If-Match": {`"other"`}}, 412},
		{"if none match", http.Header{"If-None-Match": {`"current"`}}, 304},
		{"if none weak", http.Header{"If-None-Match": {`W/"current"`}}, 304},
		{"if none different", http.Header{"If-None-Match": {`"other"`}}, 0},
		{"if none star", http.Header{"If-None-Match": {"*"}}, 304},
		{"multiple fields", http.Header{"If-None-Match": {`"other"`}, "if-none-match": {`"current"`}}, 304},
		{"malformed suffix", http.Header{"If-None-Match": {`"current", broken`}}, 400},
		{"unterminated", http.Header{"If-None-Match": {`"current`}}, 400},
		{"wildcard list", http.Header{"If-None-Match": {`*, "current"`}}, 400},
		{"invalid weak prefix", http.Header{"If-Match": {`w/"current"`}}, 400},
		{"empty tag list", http.Header{"If-Match": {" , "}}, 400},
		{"match precedence", http.Header{"If-Match": {`"other"`}, "If-None-Match": {`"current"`}}, 412},
		{"match skips date", http.Header{"If-Match": {"*"}, "If-Unmodified-Since": {"Tue, 20 Oct 2015 07:28:00 GMT"}}, 0},
		{"none skips date", http.Header{"If-None-Match": {`"other"`}, "If-Modified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT"}}, 0},
		{"modified since", http.Header{"If-Modified-Since": {"Tue, 20 Oct 2015 07:28:00 GMT"}}, 0},
		{"unmodified since", http.Header{"If-Modified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT"}}, 304},
		{"failed unmodified", http.Header{"If-Unmodified-Since": {"Tue, 20 Oct 2015 07:28:00 GMT"}}, 412},
		{"invalid date", http.Header{"If-Modified-Since": {"invalid"}}, 0},
		{"multiple dates", http.Header{"If-Modified-Since": {"Wed, 21 Oct 2015 07:28:00 GMT", "Wed, 21 Oct 2015 07:28:00 GMT"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.h.Clone()
			if got := Status(tc.h, selected); got != tc.want {
				t.Fatalf("status=%d, want %d", got, tc.want)
			}
			if !reflect.DeepEqual(tc.h, before) {
				t.Fatal("request headers mutated")
			}
		})
	}
}

func TestTagSyntaxAndMissingValidators(t *testing.T) {
	for _, tag := range []string{`"a,b"`, `"a\b"`, `""`, "\"\x80\""} {
		if got := Status(http.Header{"If-None-Match": {"W/" + tag}}, http.Header{"ETag": {tag}}); got != 304 {
			t.Fatalf("valid opaque tag %q did not match: %d", tag, got)
		}
	}
	for _, h := range []http.Header{nil, {"ETag": {`W/"current"`}}} {
		if got := Status(http.Header{"If-Match": {`"current"`}}, h); got != 412 {
			t.Fatal("missing/weak validator satisfied a strong precondition")
		}
	}
	if Status(http.Header{"If-None-Match": {"*"}}, nil) != 304 || Status(http.Header{"If-Match": {"*"}}, nil) != 0 {
		t.Fatal("wildcard existence incorrectly required a validator")
	}
}

func TestClear(t *testing.T) {
	h := http.Header{"IF-MATCH": {"*"}, "if-none-match": {"*"}, "If-Modified-Since": {"date"}, "If-Unmodified-Since": {"date"}, "rAnGe": {"bytes=0-1"}, "If-Range": {"tag"}, "Accept-Encoding": {"gzip"}}
	Clear(h)
	if !reflect.DeepEqual(h, http.Header{"Accept-Encoding": {"gzip"}}) {
		t.Fatalf("render headers: %v", h)
	}
}

func FuzzTagLists(f *testing.F) {
	for _, value := range []string{`"a"`, `W/"a"`, "*", `"a,b", "c"`, "\x00", `"a\b"`} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		status := Status(http.Header{"If-None-Match": {value}}, http.Header{"ETag": {`"a"`}})
		if status != 0 && status != 304 && status != 400 {
			t.Fatalf("unexpected read condition outcome: %d", status)
		}
	})
}
