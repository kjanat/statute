//go:build e2e

// Package report is the wire schema between the e2e harness and the
// client actor: the harness writes a Plan onto the shared mount, the
// client executes it and writes a Report back. Both sides marshal plain
// JSON so the artifacts stay readable on their own.
package report

import "time"

// Plan is one client's complete request plan for one scenario run.
type Plan struct {
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	Topology string `json:"topology"`
	// Seed makes any randomized ordering reproducible. Steps themselves
	// execute in declaration order.
	Seed  int64  `json:"seed"`
	Steps []Step `json:"steps"`
}

// Step is one request shape, executed Count times with Concurrency
// parallel workers. TargetServer names the Statute node the URL points
// at, so edge assertions need no URL parsing.
type Step struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	TargetServer string `json:"target_server"`
	// Proto selects the client transport: "h1", "h2", or "h3".
	Proto   string            `json:"proto"`
	Method  string            `json:"method,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// RootsFile is a PEM bundle the TLS transports trust; empty means
	// system roots. ServerName overrides SNI when the URL host is not
	// the certificate name. ClientCertFile and ClientKeyFile are an
	// optional client identity presented during the handshake.
	RootsFile      string `json:"roots_file,omitempty"`
	ServerName     string `json:"server_name,omitempty"`
	ClientCertFile string `json:"client_cert_file,omitempty"`
	ClientKeyFile  string `json:"client_key_file,omitempty"`
	Count          int    `json:"count"`
	Concurrency    int    `json:"concurrency"`
	Expect         Expect `json:"expect"`
}

// Expect is the per-request success predicate. Zero values are not
// checked.
type Expect struct {
	Status       int    `json:"status,omitempty"`
	BodyContains string `json:"body_contains,omitempty"`
}

// Result is one executed request.
type Result struct {
	Step         string `json:"step"`
	URL          string `json:"url"`
	TargetServer string `json:"target_server"`
	Proto        string `json:"proto"`
	// NegotiatedProto is the protocol the response actually arrived on
	// ("HTTP/1.1", "HTTP/2.0", "HTTP/3.0").
	NegotiatedProto string `json:"negotiated_proto,omitempty"`
	Status          int    `json:"status,omitempty"`
	// OriginID is the origin instance that served the request, parsed
	// from the echo body when present.
	OriginID  string        `json:"origin_id,omitempty"`
	RequestID string        `json:"request_id,omitempty"`
	Latency   time.Duration `json:"latency_ns"`
	OK        bool          `json:"ok"`
	Err       string        `json:"err,omitempty"`
}

// Report is one client's complete outcome for one plan.
type Report struct {
	ClientID string    `json:"client_id"`
	Plan     string    `json:"plan"`
	Topology string    `json:"topology"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Results  []Result  `json:"results"`
	// Failures lists every step whose planned edge produced no
	// successful traversal; a non-empty list is a client exit failure.
	Failures []string `json:"failures,omitempty"`
}

// EdgeSuccesses counts successful results per (step, target server)
// pair, the unit the harness asserts full-mesh coverage on.
func (r *Report) EdgeSuccesses() map[[2]string]int {
	out := make(map[[2]string]int)
	for _, res := range r.Results {
		if res.OK {
			out[[2]string{res.Step, res.TargetServer}]++
		}
	}
	return out
}
