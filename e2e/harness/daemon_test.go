//go:build e2e

package harness

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestDaemonSelection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         map[string]string
		contextHost string
		wantCalls   []string
		wantError   bool
	}{
		{name: "selected context", contextHost: localDockerHost, wantCalls: []string{"context show", "context inspect selected"}},
		{name: "host override", env: map[string]string{"DOCKER_HOST": localDockerHost}},
		{name: "context overrides host", env: map[string]string{"DOCKER_CONTEXT": "explicit", "DOCKER_HOST": "tcp://remote:2375"}, contextHost: localDockerHost, wantCalls: []string{"context inspect explicit"}},
		{name: "remote context", contextHost: "ssh://remote", wantCalls: []string{"context show", "context inspect selected"}, wantError: true},
		{name: "other socket", env: map[string]string{"DOCKER_HOST": "unix:///tmp/docker.sock"}, wantError: true},
		{name: "TLS configuration", env: map[string]string{"DOCKER_TLS_VERIFY": "1"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			host, err := selectDaemon(t.Context(), func(key string) string { return tc.env[key] }, func(_ context.Context, args ...string) ([]byte, error) {
				call := strings.Join(args, " ")
				calls = append(calls, call)
				if call == "context show" {
					return []byte("selected\n"), nil
				}
				return fmt.Appendf(nil, `[{"Endpoints":{"docker":{"Host":%q}}}]`, tc.contextHost), nil
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("host=%q err=%v", host, err)
			}
			if !tc.wantError && host != localDockerHost {
				t.Errorf("host=%q", host)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("calls=%v want=%v", calls, tc.wantCalls)
			}
		})
	}
}

func TestComposeFreezesDaemon(t *testing.T) {
	t.Setenv("DOCKER_CONTEXT", "remote")
	t.Setenv("DOCKER_HOST", "ssh://remote")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_API_VERSION", "1.10")
	c := &Compose{Engine: &Engine{host: localDockerHost}, Project: "test", Env: map[string]string{"STATUTE_SCENARIO": "test", "DOCKER_CONTEXT": "other", "DOCKER_HOST": "tcp://other:2375"}}
	cmd := c.command(t.Context(), "config")
	if !slices.Equal(cmd.Args[:3], []string{"docker", "--host", localDockerHost}) {
		t.Fatalf("args=%v", cmd.Args)
	}
	for _, item := range cmd.Env {
		if strings.HasPrefix(item, "DOCKER_CONTEXT=") || strings.HasPrefix(item, "DOCKER_HOST=") || strings.HasPrefix(item, "DOCKER_TLS_VERIFY=") || strings.HasPrefix(item, "DOCKER_API_VERSION=") {
			t.Errorf("daemon override survived: %s", item)
		}
	}
}

func TestDaemonContextFailsClosed(t *testing.T) {
	for _, payload := range []string{
		`invalid`,
		`[]`,
		`[{"Endpoints":{}}]`,
		`[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock","SkipTLSVerify":true}}}]`,
		`[{"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock"}},"TLSMaterial":{"docker":["key.pem"]}}]`,
	} {
		if _, err := contextDaemon("test", []byte(payload)); err == nil {
			t.Errorf("accepted context %s", payload)
		}
	}
	_, err := selectDaemon(t.Context(), func(string) string { return "" }, func(context.Context, ...string) ([]byte, error) { return nil, fmt.Errorf("context lookup failed") })
	if err == nil || !strings.Contains(err.Error(), "context lookup failed") {
		t.Fatalf("lookup failure lost: %v", err)
	}
}
