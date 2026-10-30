package migrate

import (
	"path/filepath"
	"reflect"
	"testing"
)

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
