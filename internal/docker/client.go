// Package docker is a minimal Docker Engine API client plus the label
// extraction logic that turns container labels into statute service
// registrations. It speaks only the endpoints the docker provider needs
// (container listing, inspection, start/stop, and the event stream) over a
// unix socket or TCP, with no dependency on the Docker SDK.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Client talks to the Docker Engine API.
type Client struct {
	// baseURL is the http(s) URL requests are issued against. For unix
	// sockets this is a placeholder host; the transport dials the socket.
	baseURL string
	http    *http.Client
}

// Transport-level deadlines. http.Client.Timeout is deliberately unset —
// /events is a long-lived stream — so the guards live on the transport:
// the daemon sends response headers immediately, making a header timeout
// safe even for the event stream, while a silent daemon can no longer
// hang a reconcile forever.
const (
	dialTimeout           = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// NewClient builds a client for the given endpoint. Supported forms:
//
//	unix:///var/run/docker.sock
//	tcp://127.0.0.1:2375
//	http://127.0.0.1:2375
func NewClient(endpoint string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("docker endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "unix":
		socketPath := u.Path
		if u.Host != "" {
			// unix://var/run/... parses the first segment as host.
			socketPath = "/" + u.Host + u.Path
		}
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: dialTimeout}
				return d.DialContext(ctx, "unix", socketPath)
			},
			ResponseHeaderTimeout: responseHeaderTimeout,
		}
		return &Client{
			baseURL: "http://docker",
			http:    &http.Client{Transport: transport},
		}, nil
	case "tcp", "http":
		return &Client{
			baseURL: "http://" + u.Host,
			http: &http.Client{Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
				ResponseHeaderTimeout: responseHeaderTimeout,
			}},
		}, nil
	default:
		return nil, fmt.Errorf("docker endpoint %q: unsupported scheme %q (use unix://, tcp://, or http://)", endpoint, u.Scheme)
	}
}

// Container is the subset of the Docker container listing statute needs.
type Container struct {
	ID     string
	Name   string
	Labels map[string]string
	// Networks maps docker network name to the container's IP on it.
	Networks map[string]string
	// Ports is the deduplicated, sorted list of private (container-side)
	// TCP ports the container exposes.
	Ports []int
	// Running reports whether the container's state is "running". A
	// stopped container has no network IP and lists no ports.
	Running bool
}

// containerJSON mirrors the wire format of GET /containers/json.
type containerJSON struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Labels map[string]string
	Ports  []struct {
		PrivatePort int    `json:"PrivatePort"`
		Type        string `json:"Type"`
	}
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		}
	}
}

// ListContainers returns all containers, running and stopped alike. The
// caller decides what a stopped container may contribute.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	q := url.Values{}
	q.Set("all", "true")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/containers/json?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: list containers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker: list containers: unexpected status %s", resp.Status)
	}
	var raw []containerJSON
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("docker: list containers: decode: %w", err)
	}
	out := make([]Container, 0, len(raw))
	for _, cj := range raw {
		out = append(out, normalizeContainer(cj))
	}
	return out, nil
}

// normalizeContainer converts the wire form to the internal Container.
func normalizeContainer(cj containerJSON) Container {
	name := cj.ID
	if len(cj.Names) > 0 {
		name = strings.TrimPrefix(cj.Names[0], "/")
	}
	networks := make(map[string]string, len(cj.NetworkSettings.Networks))
	for netName, n := range cj.NetworkSettings.Networks {
		if n.IPAddress != "" {
			networks[netName] = n.IPAddress
		}
	}
	seen := map[int]bool{}
	var ports []int
	for _, p := range cj.Ports {
		if p.Type != "" && p.Type != "tcp" {
			continue
		}
		if p.PrivatePort > 0 && !seen[p.PrivatePort] {
			seen[p.PrivatePort] = true
			ports = append(ports, p.PrivatePort)
		}
	}
	sort.Ints(ports)
	return Container{
		ID:       cj.ID,
		Name:     name,
		Labels:   cj.Labels,
		Networks: networks,
		Ports:    ports,
		Running:  cj.State == "running",
	}
}

// InspectState is the lifecycle subset of GET /containers/{id}/json.
type InspectState struct {
	Running bool
	// Health is the HEALTHCHECK status: "starting", "healthy", or
	// "unhealthy". Empty when the container defines no HEALTHCHECK.
	Health string
}

// InspectContainer returns the container's current lifecycle state.
func (c *Client) InspectContainer(ctx context.Context, id string) (InspectState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return InspectState{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return InspectState{}, fmt.Errorf("docker: inspect container: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return InspectState{}, fmt.Errorf("docker: inspect container: unexpected status %s", resp.Status)
	}
	var raw struct {
		State struct {
			Running bool
			Health  *struct {
				Status string
			}
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return InspectState{}, fmt.Errorf("docker: inspect container: decode: %w", err)
	}
	out := InspectState{Running: raw.State.Running}
	if raw.State.Health != nil {
		out.Health = raw.State.Health.Status
	}
	return out, nil
}

// StartContainer starts the container. A container that is already running
// is not an error.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.lifecyclePost(ctx, id, "start")
}

// StopContainer stops the container with the daemon's default grace
// period. A container that is already stopped is not an error.
func (c *Client) StopContainer(ctx context.Context, id string) error {
	return c.lifecyclePost(ctx, id, "stop")
}

// lifecyclePost issues one container lifecycle POST. 204 is success and
// 304 means the container is already in the requested state.
func (c *Client) lifecyclePost(ctx context.Context, id, action string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/containers/"+url.PathEscape(id)+"/"+action, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: %s container: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("docker: %s container: unexpected status %s", action, resp.Status)
	}
	return nil
}

// Event is a Docker Engine event. Only the discriminating fields are kept.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// ActorName is the container name the event describes, or "".
func (e Event) ActorName() string {
	return e.Actor.Attributes["name"]
}

// typeContainer is the Event.Type value for container lifecycle events.
const typeContainer = "container"

// topologyActions are the container lifecycle actions that can change the
// set of routable backends.
var topologyActions = map[string]bool{
	"start":   true,
	"die":     true,
	"stop":    true,
	"kill":    true,
	"pause":   true,
	"unpause": true,
	"restart": true,
	"update":  true,
	"rename":  true,
}

// ChangesTopology reports whether the event can alter routing state.
func (e Event) ChangesTopology() bool {
	if e.Type != typeContainer {
		return false
	}
	// Health transitions arrive as "health_status: healthy".
	return topologyActions[e.Action] || strings.HasPrefix(e.Action, "health_status")
}

// StreamEvents opens the event stream and invokes handle for every event
// until the stream ends or ctx is cancelled. It returns the stream error;
// the caller owns reconnect policy.
func (c *Client) StreamEvents(ctx context.Context, handle func(Event)) error {
	q := url.Values{}
	q.Set("filters", `{"type":["container"]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/events?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: event stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: event stream: unexpected status %s", resp.Status)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			// Context cancellation surfaces as a read error on the body.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("docker: event stream: decode: %w", err)
		}
		handle(ev)
	}
}

// Ping checks that the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: ping: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker: ping: unexpected status %s", resp.Status)
	}
	return nil
}
