package statute

import "testing"

func TestTLSNameIndex(t *testing.T) {
	t.Parallel()
	var index tlsNameIndex[int]
	if _, found := index.lookup("a.example"); found {
		t.Fatal("empty index matched")
	}
	for pattern, value := range map[string]int{"*.EXAMPLE..": 1, "a.example": 2, "BÜCHER.example。": 3, "_legacy.example": 4} {
		if err := index.add(pattern, value); err != nil {
			t.Fatal(err)
		}
	}
	for host, want := range map[string]int{"A.EXAMPLE..": 2, "b.example": 1, "bücher.example": 3, "xn--bcher-kva.example.": 3, "_LEGACY.example": 4} {
		if got, found := index.lookup(host); !found || got != want {
			t.Errorf("%q: (%d, %v), want (%d, true)", host, got, found, want)
		}
	}
	for _, host := range []string{"", "example", "deep.b.example", "elsewhere.test"} {
		if got, found := index.lookup(host); found {
			t.Errorf("%q: unexpectedly found %d", host, got)
		}
	}
}

func TestTLSNameIndexRejectsDuplicates(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{{"a.example", "A.example。"}, {"*.EXAMPLE..", "*.example."}, {"bücher.example", "xn--bcher-kva.example"}} {
		var index tlsNameIndex[int]
		if err := index.add(pair[0], 1); err != nil {
			t.Fatal(err)
		}
		if err := index.add(pair[1], 99); err == nil {
			t.Errorf("duplicate %q overwritten", pair[1])
		}
		if got, _ := index.lookup(pair[0]); got != 1 {
			t.Error("duplicate attempt mutated the index")
		}
	}
}
