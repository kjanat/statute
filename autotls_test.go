package statute

import (
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
			AutoTLSSources: []*resolved.AutoTLS{{
				Domains: []string{"a.example.com", "b.example.com"},
				Email:   "ops@example.com",
				Storage: "/tmp/x",
			}},
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
			AutoTLSSources: []*resolved.AutoTLS{{Domains: []string{"a"}, Email: "a@x", Storage: "/tmp/a"}},
		},
		{
			Addr: ":8443", Scheme: "https",
			AutoTLSSources: []*resolved.AutoTLS{{Domains: []string{"b"}, Email: "b@x", Storage: "/tmp/a"}},
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
			AutoTLSSources: []*resolved.AutoTLS{{Domains: []string{"a"}, Email: "a@x", Storage: "/tmp/a"}},
		},
		{
			Addr: ":8443", Scheme: "https",
			AutoTLSSources: []*resolved.AutoTLS{{Domains: []string{"b"}, Email: "a@x", Storage: "/tmp/b"}},
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

// TestBuildAutocertManager_EmptyStorageErrors — an HTTP-01 source with no
// storage path cannot run ACME persistently.
func TestBuildAutocertManager_EmptyStorageErrors(t *testing.T) {
	t.Parallel()
	listeners := []*resolved.Listener{{
		Addr: ":443", Scheme: "https",
		AutoTLSSources: []*resolved.AutoTLS{{Domains: []string{"a"}, Email: "a@x"}},
	}}
	_, err := buildAutocertManager(listeners)
	if err == nil || !strings.Contains(err.Error(), "storage path is required") {
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
			AutoTLSSources: []*resolved.AutoTLS{{
				Domains: []string{"a"},
				Email:   "a@x",
				Storage: "/tmp/a",
				DNS01:   &resolved.CloudflareDNS01{APIToken: "tok"},
			}},
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
