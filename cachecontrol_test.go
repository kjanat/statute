package statute

import (
	"net/http"
	"testing"
)

func TestCacheControlAllowsStorage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"absent", nil, true},
		{"empty members", []string{" , , \t", ""}, true},
		{"ordinary", []string{"public, max-age=60", `extension="a,b"`}, true},
		{"bare", []string{"no-store"}, false},
		{"mixed case", []string{"public, No-StOrE, max-age=60"}, false},
		{"separate field", []string{"public", "max-age=60", "no-store"}, false},
		{"whitespace", []string{"\t no-store \t,"}, false},
		{"argument cannot enable storage", []string{`no-store="false"`}, false},
		{"name prefix", []string{"x-no-store, no-store-extension"}, true},
		{"token argument", []string{"extension=no-store"}, true},
		{"quoted directive", []string{`extension="public, no-store"`}, true},
		{"quoted escape", []string{`extension="a\", no-store"`}, true},
		{"after quoted comma", []string{`extension="a,b", no-store`}, false},
		{"after escaped slash", []string{`extension="a\\", no-store`}, false},
		{"unterminated quote", []string{`extension="a, no-store`}, false},
		{"unterminated escape", []string{`extension="a\`}, false},
		{"unseparated directives", []string{"public no-store"}, false},
		{"invalid token", []string{"extension=(value)"}, false},
		{"invalid name", []string{"bad:name=value"}, false},
		{"missing argument", []string{"extension="}, false},
		{"quote suffix", []string{`extension="value"suffix`}, false},
		{"control in quote", []string{"extension=\"a\x00b\""}, false},
		{"escaped control", []string{"extension=\"a\\\x00b\""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{"Cache-Control": tc.values}
			if got := cacheControlAllowsStorage(h); got != tc.want {
				t.Errorf("%q: got %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

func TestCacheControlNonCanonicalFieldNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"cache-control", "CACHE-CONTROL", "cAcHe-CoNtRoL"} {
		h := http.Header{"Cache-Control": {"public"}, name: {"no-store"}}
		if cacheControlAllowsStorage(h) {
			t.Fatalf("missed non-canonical field %q", name)
		}
	}
}

func FuzzCacheControlNoStore(f *testing.F) {
	for _, seed := range []string{"", "public", `ext="a,b"`, `ext="a\"b"`, "no-store", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		// A separate no-store field always vetoes storage, whatever its peers.
		for _, fields := range [][]string{{value, "no-store"}, {"no-store", value}} {
			if cacheControlAllowsStorage(http.Header{"Cache-Control": fields}) {
				t.Fatalf("no-store lost in %q", fields)
			}
		}
		// Appending a directive to any accepted field preserves its boundary.
		if cacheControlFieldAllowsStorage(value) && cacheControlFieldAllowsStorage(value+", no-store") {
			t.Fatalf("no-store lost after %q", value)
		}
	})
}
