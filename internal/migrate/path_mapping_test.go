package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProviderInventoryRespectsNestedMountShadowing(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	static := write("root/traefik.yml", "entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\nproviders:\n  file: {filename: /etc/traefik/dynamic/routes.yml}\n")
	hidden := write("root/dynamic/routes.yml", simpleDynamic)
	actual := write("actual-dynamic/routes.yml", simpleDynamic)
	for _, reverse := range []bool{false, true} {
		mounts := []string{"./root:/etc/traefik", "./actual-dynamic:/etc/traefik/dynamic"}
		if reverse {
			mounts[0], mounts[1] = mounts[1], mounts[0]
		}
		compose := write("compose.yml", fmt.Sprintf("services:\n  traefik:\n    volumes: [%q, %q]\n", mounts[0], mounts[1]))
		for _, path := range []string{hidden, actual} {
			r := audit(t, Inputs{Static: []string{static}, Dynamic: []string{path}, Compose: []string{compose}})
			if r.Report.Blocking != (path == hidden) {
				t.Fatalf("reverse=%t input=%s blocking=%t findings=%s", reverse, path, r.Report.Blocking, findings(r))
			}
		}
	}
}

func TestProviderPathsFollowStaticOwnerMounts(t *testing.T) {
	root := t.TempDir()
	static := filepath.Join(root, "static", "traefik.yml")
	r := &Result{inputs: Inputs{Static: []string{static}}}
	document := map[string]any{"services": map[string]any{
		"traefik": map[string]any{"volumes": []any{
			filepath.Dir(static) + ":/etc/traefik:ro",
			map[string]any{"type": "bind", "source": "./dynamic", "target": "/config"},
			"cache:/cache", "invalid",
			map[string]any{"type": "volume", "source": "cache", "target": "/data"},
			"${PRIVATE_SOURCE}:/ignored", "/host:relative",
		}},
		"unrelated": map[string]any{"volumes": []any{"/unrelated:/etc/traefik:ro"}},
	}}
	r.inventoryMounts(filepath.Join(root, "compose.yml"), document)
	g := &generator{r: r}
	if got := g.mappedProviderPaths("/etc/traefik/traefik.yml"); !reflect.DeepEqual(got, []string{static}) {
		t.Fatalf("static path mapping=%v", got)
	}
	want := filepath.Join(root, "dynamic", "routes.yml")
	if got := g.mappedProviderPaths("/config/routes.yml"); !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("relative bind mapping=%v", got)
	}
	if len(g.mappedProviderPaths("/etc/traefik-other/routes.yml")) != 0 {
		t.Fatal("mount prefix escaped directory boundary")
	}
	r.inputs.PathMappings = map[string]string{"/config": filepath.Join(root, "explicit"), "/config/sub": filepath.Join(root, "specific")}
	if got := g.mappedProviderPaths("/config/sub/routes.yml"); !reflect.DeepEqual(got, []string{filepath.Join(root, "specific", "routes.yml")}) {
		t.Fatalf("explicit mapping precedence=%v", got)
	}
}

func TestMappedProviderInventoryAcceptsSuppliedFile(t *testing.T) {
	in := basicInputs(t, simpleDynamic)
	in.Static = []string{fixture(t, "static.yml", "entryPoints:\n  web: {address: ':8080', http: {sanitizePath: false}}\nproviders:\n  file: {filename: /container/routes.yml}\n")}
	in.PathMappings = map[string]string{"/container/routes.yml": in.Dynamic[0]}
	requireMigrationGaps(t, audit(t, in))
}
