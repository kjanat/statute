package statute

import (
	"runtime/debug"
	"testing"
)

func TestCleanVersion(t *testing.T) {
	cases := map[string]string{
		"v0.3.0":     "0.3.0",
		"v1.2.3-rc1": "1.2.3-rc1",
		"0.3.0":      "0.3.0", // already clean
		"(devel)":    "",
		"":           "",
	}
	for in, want := range cases {
		if got := cleanVersion(in); got != want {
			t.Errorf("cleanVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	t.Run("no build info", func(t *testing.T) {
		if got := resolveVersion(nil, false); got != enumUnknown {
			t.Errorf("got %q, want %q", got, enumUnknown)
		}
	})

	t.Run("statute as main module", func(t *testing.T) {
		bi := &debug.BuildInfo{Main: debug.Module{Path: statuteModulePath, Version: "v0.4.0"}}
		if got := resolveVersion(bi, true); got != "0.4.0" {
			t.Errorf("got %q, want 0.4.0", got)
		}
	})

	t.Run("statute as pinned dependency", func(t *testing.T) {
		bi := &debug.BuildInfo{
			Main: debug.Module{Path: "example.com/user/app", Version: goDevelVersion},
			Deps: []*debug.Module{
				{Path: "golang.org/x/crypto", Version: "v0.1.0"},
				{Path: statuteModulePath, Version: "v0.5.0"},
			},
		}
		if got := resolveVersion(bi, true); got != "0.5.0" {
			t.Errorf("got %q, want 0.5.0", got)
		}
	})

	t.Run("local checkout falls back to VCS revision", func(t *testing.T) {
		bi := &debug.BuildInfo{
			Main: debug.Module{Path: statuteModulePath, Version: goDevelVersion},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "9263409a1b2c3d4e5f6071829304a5b6c7d8e9f0"},
				{Key: "vcs.modified", Value: "true"},
			},
		}
		if got := resolveVersion(bi, true); got != "9263409a1b2c-dirty" {
			t.Errorf("got %q, want 9263409a1b2c-dirty", got)
		}
	})

	t.Run("no version and no VCS stamp", func(t *testing.T) {
		bi := &debug.BuildInfo{Main: debug.Module{Path: statuteModulePath, Version: goDevelVersion}}
		if got := resolveVersion(bi, true); got != enumUnknown {
			t.Errorf("got %q, want %q", got, enumUnknown)
		}
	})
}

// TestVersion_NotEmpty is a smoke test: under `go test` the function must
// still return a non-empty value (VCS revision in CI, or "unknown").
func TestVersion_NotEmpty(t *testing.T) {
	if version() == "" {
		t.Fatal("version() returned empty string")
	}
}

// TestStatuteModulePath locks the reflect-derived module path. It fails
// if version.go is moved out of the root package (PkgPath would gain a
// subpackage suffix) or the module is renamed without updating expectations.
func TestStatuteModulePath(t *testing.T) {
	if statuteModulePath != "statute.kjanat.dev" {
		t.Fatalf("statuteModulePath = %q, want statute.kjanat.dev", statuteModulePath)
	}
}
