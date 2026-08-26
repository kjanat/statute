//go:build e2e

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// composeTimeout caps any single docker/compose invocation so a wedged
// daemon cannot absorb a scenario's whole budget, teardown included.
const composeTimeout = 2 * time.Minute

// Compose drives one uniquely named Compose project through the docker
// CLI. The CLI (not an SDK) keeps the topology declarative in the
// checked-in compose files and leaves this type responsible only for
// naming, environment, and invocation discipline.
type Compose struct {
	// Project is the unique per-run project name; compose derives
	// network and container names from it, which is what makes parallel
	// and repeated runs collision-free.
	Project string
	// Files are the compose files in override order: base, topology,
	// scenario.
	Files []string
	// Env is appended to the process environment for every invocation
	// (image reference, scenario name, artifact mount path).
	Env map[string]string
	// Dir is the working directory for relative paths in compose files.
	Dir string
}

// command builds one docker CLI invocation with the project's files and
// environment.
func (c *Compose) command(ctx context.Context, args ...string) *exec.Cmd {
	full := make([]string, 0, 3+2*len(c.Files)+len(args))
	full = append(full, "compose", "-p", c.Project)
	for _, f := range c.Files {
		full = append(full, "-f", f)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = c.Dir
	cmd.Env = os.Environ()
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
}

// Output runs one compose subcommand under the invocation timeout and
// returns its stdout. Compose writes progress noise to stderr, so
// keeping the streams separate is what makes stdout parseable; on
// failure the error carries both streams so the test log alone
// diagnoses it.
func (c *Compose) Output(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := c.command(ctx, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("docker compose %s: %w\n%s%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String(), err
}

// Up starts the named services (or the whole project) detached and
// waits for containers to be running — not for the application to be
// ready; readiness is proven separately over the network.
func (c *Compose) Up(ctx context.Context, services ...string) error {
	args := append([]string{"up", "-d", "--wait", "--wait-timeout", "60"}, services...)
	_, err := c.Output(ctx, args...)
	return err
}

// UpDetached starts services without waiting for a running state — for
// containers expected to exit, such as a Statute node whose startup
// must fail.
func (c *Compose) UpDetached(ctx context.Context, services ...string) error {
	args := append([]string{"up", "-d"}, services...)
	_, err := c.Output(ctx, args...)
	return err
}

// RunClient executes one client-actor invocation as a one-shot
// container on the project network and returns its output.
func (c *Compose) RunClient(ctx context.Context, service string, clientArgs ...string) (string, error) {
	args := append([]string{"run", "--rm", "--no-deps", service}, clientArgs...)
	return c.Output(ctx, args...)
}

// Signal delivers a signal to one service's container.
func (c *Compose) Signal(ctx context.Context, service, signal string) error {
	_, err := c.Output(ctx, "kill", "-s", signal, service)
	return err
}

// Stop stops one service and waits up to timeout for it to exit.
func (c *Compose) Stop(ctx context.Context, service string, timeout time.Duration) error {
	_, err := c.Output(ctx, "stop", "-t", fmt.Sprintf("%d", int(timeout.Seconds())), service)
	return err
}

// Restart restarts one service and waits for its container to run.
func (c *Compose) Restart(ctx context.Context, service string) error {
	_, err := c.Output(ctx, "restart", "-t", "20", service)
	return err
}

// Down removes the project's containers, network, and volumes.
func (c *Compose) Down(ctx context.Context) error {
	_, err := c.Output(ctx, "down", "-v", "--remove-orphans", "-t", "20")
	return err
}
