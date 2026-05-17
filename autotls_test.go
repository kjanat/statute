package statute

import (
	"slices"
	"strings"
	"testing"

	"statute.kjanat.dev/resolved"
)

func TestBuildAutocertManager_None(t *testing.T) {
	t.Parallel()
	m, err := buildAutocertManager(nil)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("manager: got non-nil for no AutoTLS listeners")
	}
}

func TestBuildAutocertManager_SingleListener(t *testing.T) {
	t.Parallel()
	listeners := []*resolved.Listener{
		{
			Addr:   ":443",
			Scheme: "https",
			AutoTLS: &resolved.AutoTLS{
				Domains: []string{"a.example.com", "b.example.com"},
				Email:   "ops@example.com",
				Storage: "/tmp/x",
			},
		},
	}
	m, err := buildAutocertManager(listeners)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("want manager, got nil")
		return
	}
	if m.Email != "ops@example.com" {
		t.Errorf("email: got %q", m.Email)
	}
}

func TestBuildAutocertManager_MismatchedEmailErrors(t *testing.T) {
	t.Parallel()
	listeners := []*resolved.Listener{
		{
			Addr: ":443", Scheme: "https",
			AutoTLS: &resolved.AutoTLS{Domains: []string{"a"}, Email: "a@x", Storage: "/tmp/a"},
		},
		{
			Addr: ":8443", Scheme: "https",
			AutoTLS: &resolved.AutoTLS{Domains: []string{"b"}, Email: "b@x", Storage: "/tmp/a"},
		},
	}
	_, err := buildAutocertManager(listeners)
	if err == nil {
		t.Fatal("want error for mismatched email")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error: %v", err)
	}
}

func TestBuildAutocertManager_MismatchedStorageErrors(t *testing.T) {
	t.Parallel()
	listeners := []*resolved.Listener{
		{
			Addr: ":443", Scheme: "https",
			AutoTLS: &resolved.AutoTLS{Domains: []string{"a"}, Email: "a@x", Storage: "/tmp/a"},
		},
		{
			Addr: ":8443", Scheme: "https",
			AutoTLS: &resolved.AutoTLS{Domains: []string{"b"}, Email: "a@x", Storage: "/tmp/b"},
		},
	}
	_, err := buildAutocertManager(listeners)
	if err == nil {
		t.Fatal("want error for mismatched storage")
	}
	if !strings.Contains(err.Error(), "storage") {
		t.Errorf("error: %v", err)
	}
}

// TestBuildAutocertManager_DNS01ExcludedFromManager — listeners that use
// DNS-01 don't contribute their domains to the shared autocert manager;
// they get their own per-listener manager.
func TestBuildAutocertManager_DNS01ExcludedFromManager(t *testing.T) {
	t.Parallel()
	listeners := []*resolved.Listener{
		{
			Addr: ":443", Scheme: "https",
			AutoTLS: &resolved.AutoTLS{
				Domains: []string{"a"},
				Email:   "a@x",
				Storage: "/tmp/a",
				DNS01:   &resolved.CloudflareDNS01{APIToken: "tok"},
			},
		},
	}
	m, err := buildAutocertManager(listeners)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("manager: got non-nil; DNS-01 listeners should not contribute")
	}
}

func TestAutocertTLSConfig_NextProtos(t *testing.T) {
	t.Parallel()
	listeners := []*resolved.Listener{{
		Addr: ":443", Scheme: "https",
		AutoTLS: &resolved.AutoTLS{Domains: []string{"a"}, Email: "x@x", Storage: "/tmp/a"},
	}}
	m, err := buildAutocertManager(listeners)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		http2, cf bool
		want      []string
	}{
		{"h1 only, public", false, false, []string{"http/1.1", "acme-tls/1"}},
		{"h2, public", true, false, []string{"h2", "http/1.1", "acme-tls/1"}},
		{"h2, behind cloudflare", true, true, []string{"h2", "http/1.1"}},
		{"h1, behind cloudflare", false, true, []string{"http/1.1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := autocertTLSConfig(m, c.http2, c.cf)
			if !slices.Equal(cfg.NextProtos, c.want) {
				t.Errorf("NextProtos: got %v, want %v", cfg.NextProtos, c.want)
			}
		})
	}
}
