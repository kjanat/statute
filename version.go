package statute

import (
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
)

// Build-setting keys stamped by the Go toolchain for VCS-tracked builds.
const (
	vcsRevisionKey = "vcs.revision"
	vcsModifiedKey = "vcs.modified"
)

// statuteModulePath is statute's module path, used to locate statute's
// own version inside the build info of whatever binary embeds it. It is
// derived from the import path of a type declared in this package rather
// than hand-written, so it tracks module renames automatically. This
// equals the module path only because version.go lives in the module's
// root package; moving it into a subpackage would require revisiting
// this (see TestStatuteModulePath).
var statuteModulePath = reflect.TypeFor[Config]().PkgPath()

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
	if !ok || bi == nil {
		return enumUnknown
	}
	// statute as the main module (its own examples/binaries). The VCS
	// stamp lives in bi.Settings and describes the main module, so it is
	// only statute's revision in this branch.
	if bi.Main.Path == statuteModulePath {
		if v := cleanVersion(bi.Main.Version); v != "" {
			return v
		}
		if rev := vcsRevision(bi); rev != "" {
			return rev
		}
		return enumUnknown
	}
	// statute as a dependency of the embedding binary. Deliberately does
	// NOT fall back to bi.Settings: that is the host app's VCS, not
	// statute's, and would mis-stamp statute.version.
	return versionFromDeps(bi.Deps)
}

// versionFromDeps resolves statute's version from the dependency list,
// honouring a local `replace`, or enumUnknown when it cannot be told.
func versionFromDeps(deps []*debug.Module) string {
	for _, d := range deps {
		if d == nil || d.Path != statuteModulePath {
			continue
		}
		if v := cleanVersion(d.Version); v != "" {
			return v
		}
		if d.Replace != nil {
			if v := cleanVersion(d.Replace.Version); v != "" {
				return v
			}
		}
		return enumUnknown
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
		case vcsRevisionKey:
			rev = s.Value
		case vcsModifiedKey:
			dirty, _ = strconv.ParseBool(s.Value)
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
