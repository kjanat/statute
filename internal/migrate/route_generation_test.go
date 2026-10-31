package migrate

import (
	"strings"
	"testing"
)

func TestGeneratedRoutesPreserveBytePrefixAndPeerRestrictions(t *testing.T) {
	body := strings.Replace(simpleDynamic, "Host(`api.example`) && Path(`/health`)", "Host(`api.example`) && PathPrefix(`/api`) && ClientIP(`192.0.2.0/24`)", 1)
	r := audit(t, basicInputs(t, body))
	if r.Report.Blocking || r.Report.GeneratedRoutes == 0 {
		t.Fatalf("supported route blocked: %s", findings(r))
	}
	if !strings.Contains(string(r.Go), "MatchPrefix") || !strings.Contains(string(r.Go), `.PeerIPs("192.0.2.0/24")`) {
		t.Fatal("generated route lost its prefix or peer restriction")
	}
	assertGeneratedParity(t, r)
}

func TestEmptyBackendMigrationGeneratesUnavailablePool(t *testing.T) {
	body := strings.Replace(simpleDynamic, "servers:\n          - url: http://127.0.0.1:9000", "servers: []", 1)
	r := audit(t, basicInputs(t, body))
	if r.Report.Blocking || !r.Config.Upstreams["api@file"].AllowEmpty {
		t.Fatalf("empty backend lost its unavailable-pool policy: %s", findings(r))
	}
	assertGeneratedParity(t, r)
}

func TestDashboardRouteIsExcludedFromProxyMigration(t *testing.T) {
	body := strings.Replace(simpleDynamic, "service: api", "service: api@internal", 1)
	r := audit(t, basicInputs(t, body))
	if r.Report.GeneratedRoutes != 0 {
		t.Fatal("Traefik dashboard was exposed as a proxy route")
	}
	for _, object := range r.Report.Objects {
		if object.Kind == "routers" && object.Classification != OutOfScope {
			t.Fatal("dashboard router was not classified out of scope")
		}
	}
}
