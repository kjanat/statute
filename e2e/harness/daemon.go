//go:build e2e

package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const localDockerHost = "unix:///var/run/docker.sock"

type dockerContext struct {
	Endpoints map[string]struct {
		Host          string
		SkipTLSVerify bool
	}
	TLSMaterial map[string][]string
}

// resolveDaemon applies Docker CLI context precedence before freezing its host.
func resolveDaemon(ctx context.Context) (string, error) {
	return selectDaemon(ctx, os.Getenv, func(ctx context.Context, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, composeTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, out)
		}
		return out, nil
	})
}

func selectDaemon(ctx context.Context, getenv func(string) string, command func(context.Context, ...string) ([]byte, error)) (string, error) {
	if getenv("DOCKER_TLS") != "" || getenv("DOCKER_TLS_VERIFY") != "" || getenv("DOCKER_CERT_PATH") != "" {
		return "", fmt.Errorf("e2e requires the local Docker socket; TLS environment configuration is unsupported")
	}
	name := getenv("DOCKER_CONTEXT")
	if name == "" && getenv("DOCKER_HOST") != "" {
		return validateDaemonHost(getenv("DOCKER_HOST"))
	}
	if name == "" {
		out, err := command(ctx, "context", "show")
		if err != nil {
			return "", err
		}
		name = strings.TrimSpace(string(out))
	}
	out, err := command(ctx, "context", "inspect", name)
	if err != nil {
		return "", err
	}
	return contextDaemon(name, out)
}

func contextDaemon(name string, out []byte) (string, error) {
	var contexts []dockerContext
	if err := json.Unmarshal(out, &contexts); err != nil {
		return "", fmt.Errorf("docker context %q: %w", name, err)
	}
	if len(contexts) != 1 {
		return "", fmt.Errorf("docker context %q: expected one context", name)
	}
	endpoint, ok := contexts[0].Endpoints["docker"]
	if !ok || endpoint.SkipTLSVerify || len(contexts[0].TLSMaterial["docker"]) != 0 {
		return "", fmt.Errorf("docker context %q must use the local socket without TLS", name)
	}
	return validateDaemonHost(endpoint.Host)
}

func validateDaemonHost(host string) (string, error) {
	if host != localDockerHost && host != "unix:///run/docker.sock" {
		return "", fmt.Errorf("unsupported Docker endpoint %q: e2e requires %s", host, localDockerHost)
	}
	if host != localDockerHost {
		actual, err := filepath.EvalSymlinks(strings.TrimPrefix(host, "unix://"))
		if err != nil {
			return "", fmt.Errorf("resolve Docker socket: %w", err)
		}
		expected, err := filepath.EvalSymlinks("/var/run/docker.sock")
		if err != nil || actual != expected {
			return "", fmt.Errorf("docker socket %q differs from /var/run/docker.sock", host)
		}
	}
	return localDockerHost, nil
}

func daemonEnvironment(env []string) []string {
	out := make([]string, 0, len(env))
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		switch key {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION":
		default:
			out = append(out, value)
		}
	}
	return out
}
