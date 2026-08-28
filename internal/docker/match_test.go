package docker

import "testing"

func TestMatchTraefikHostPreservesConfiguredDot(t *testing.T) {
	mt := m("example.com.", "/*")
	for _, host := range []string{"example.com", "example.com.", "example.com..", "EXAMPLE.COM"} {
		if !mt.Match(host, "/") {
			t.Errorf("Host(`example.com.`) missed %q", host)
		}
	}
	if mt.Match("example.com...", "/") {
		t.Error("Host(`example.com.`) matched three trailing dots")
	}
	if mt.Match("other.example.com", "/") {
		t.Error("Host(`example.com.`) matched a different host")
	}
}

func TestMatchNativeHostIsExact(t *testing.T) {
	mt := native("example.com.", "/*")
	if !mt.Match("example.com.", "/") || !mt.Match("EXAMPLE.COM.", "/") {
		t.Fatal("native host missed its own spelling")
	}
	if mt.Match("example.com", "/") || mt.Match("example.com..", "/") {
		t.Fatal("native host folded a trailing dot")
	}
}

func TestMatchPathKinds(t *testing.T) {
	seg := Matcher{Path: "/api/*", PathKind: PathSegment}
	bytep := Matcher{Path: "/api", PathKind: PathByte}
	exact := Matcher{Path: "/api", PathKind: PathExact}

	if !seg.Match("", "/api") || !seg.Match("", "/api/") || !seg.Match("", "/api/x") {
		t.Fatal("segment prefix missed /api family")
	}
	if seg.Match("", "/api-secret") {
		t.Fatal("segment prefix matched /api-secret")
	}
	if !bytep.Match("", "/api-secret") || !bytep.Match("", "/api") {
		t.Fatal("byte prefix missed Traefik PathPrefix traffic")
	}
	if exact.Match("", "/api/") || exact.Match("", "/api-secret") {
		t.Fatal("exact path over-matched")
	}
}

func TestTraefikHostContainment(t *testing.T) {
	undotted := m("0", "/*")
	dotted := m("0.", "/*")
	if !matcherLE(undotted, dotted) {
		t.Fatal("Host(`0`) should be a subset of Host(`0.`)")
	}
	if matcherLE(dotted, undotted) {
		t.Fatal("Host(`0.`) is not a subset of Host(`0`): request 0.. distinguishes them")
	}
}
