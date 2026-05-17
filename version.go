package statute

import (
	"runtime/debug"
	"strings"
)

// statuteModulePath is statute's module path, used to locate statute's
// own version inside the build info of whatever binary embeds it.
const statuteModulePath = "statute.kjanat.dev"

// goDevelVersion is the placeholder Go reports for a module built without
// a version (local builds, `go run`, tests).
const goDevelVersion = "(devel)"

// version reports statute's own version, derived entirely from the
// embedding binary's build info so it is always set and never drifts:
//
//   - imported and pinned (go get statute.kjanat.dev@vX.Y.Z): the pinned
//     module version, e.g. "0.3.0";
//   - built from a statute checkout (no module version): the VCS revision,
//     e.g. "9263409a1b2c" or "9263409a1b2c-dirty";
//   - no build info at all (rare): "unknown".
//
// There is deliberately no hand-maintained version constant to keep in
// sync with release tags.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	return resolveVersion(bi, ok)
}

// resolveVersion is the pure core of version, separated for testing.
func resolveVersion(bi *debug.BuildInfo, ok bool) string {
	if ok && bi != nil {
		// statute as the main module (its own examples/binaries).
		if bi.Main.Path == statuteModulePath {
			if v := cleanVersion(bi.Main.Version); v != "" {
				return v
			}
		}
		// statute as a dependency of the embedding binary.
		for _, d := range bi.Deps {
			if d != nil && d.Path == statuteModulePath {
				if v := cleanVersion(d.Version); v != "" {
					return v
				}
			}
		}
		// Local checkout with no module version: use the VCS stamp.
		if rev := vcsRevision(bi); rev != "" {
			return rev
		}
	}
	return enumUnknown
}

// cleanVersion strips the leading "v" and rejects the non-versions Go
// reports for un-tagged builds ("" and "(devel)").
func cleanVersion(v string) string {
	if v == "" || v == goDevelVersion {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// vcsRevision returns the short VCS revision from build settings, with a
// "-dirty" suffix when the working tree was modified at build time.
func vcsRevision(bi *debug.BuildInfo) string {
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
