package cloudflare

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeSnapshotStrictJSON(t *testing.T) {
	t.Parallel()
	valid := `{"ipv4":["192.0.2.0/24"],"ipv6":["2001:db8::/32"],"fetched_at":"2026-09-28T12:00:00Z"}`
	for _, tc := range []struct{ name, data string }{
		{"empty", ""},
		{"null", "null"},
		{"array", "[]"},
		{"trailing", valid + " {}"},
		{"unknown", strings.Replace(valid, `"ipv4":`, `"extra":1,"ipv4":`, 1)},
		{"duplicate", strings.Replace(valid, `"ipv4":`, `"ipv4":[],"ipv4":`, 1)},
		{"case alias", strings.Replace(valid, `"ipv4":`, `"IPv4":[],"ipv4":`, 1)},
		{"case replacement", strings.Replace(valid, `"ipv4":`, `"IPv4":`, 1)},
		{"wrong type", strings.Replace(valid, `["192.0.2.0/24"]`, `"192.0.2.0/24"`, 1)},
		{"null family", strings.Replace(valid, `["192.0.2.0/24"]`, `null`, 1)},
		{"null date", strings.Replace(valid, `"2026-09-28T12:00:00Z"`, `null`, 1)},
		{"zero date", strings.Replace(valid, `2026-09-28T12:00:00Z`, `0001-01-01T00:00:00Z`, 1)},
		{"bad date", strings.Replace(valid, `2026-09-28T12:00:00Z`, `yesterday`, 1)},
		{"missing", `{"ipv4":["192.0.2.0/24"],"ipv6":["2001:db8::/32"]}`},
		{"wrong family", strings.Replace(valid, `192.0.2.0/24`, `2001:db8::/32`, 1)},
		{"universal", strings.Replace(valid, `192.0.2.0/24`, `0.0.0.0/0`, 1)},
		{"noncanonical", strings.Replace(valid, `192.0.2.0/24`, `192.0.2.1/24`, 1)},
		{"duplicate prefix", strings.Replace(valid, `"192.0.2.0/24"`, `"192.0.2.0/24","192.0.2.0/24"`, 1)},
		{"oversized", strings.Repeat(" ", MaxSnapshotBytes) + valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := DecodeSnapshot([]byte(tc.data)); err == nil {
				t.Fatalf("invalid artifact accepted: %+v", got)
			}
		})
	}
}

func TestDecodeSnapshotNormalizes(t *testing.T) {
	t.Parallel()
	snapshot, err := DecodeSnapshot([]byte(`{
		"fetched_at":"2026-09-28T14:00:00+02:00",
		"ipv6":["2606:4700::/32","2001:db8::/32"],
		"ipv4":["198.51.100.0/24","192.0.2.0/24"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.IPv4[0] != "192.0.2.0/24" || snapshot.IPv6[0] != "2001:db8::/32" || snapshot.FetchedAt.Format(time.RFC3339) != "2026-09-28T12:00:00Z" {
		t.Fatalf("not normalized: %+v", snapshot)
	}
	normalized := snapshot.Normalized()
	normalized.IPv4[0] = "203.0.113.0/24"
	normalized.IPv6[0] = "2400:cb00::/32"
	if snapshot.IPv4[0] != "192.0.2.0/24" || snapshot.IPv6[0] != "2001:db8::/32" {
		t.Fatal("normalized snapshot aliases input")
	}
}
