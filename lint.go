package statute

import (
	"fmt"
	"strings"
	"time"

	"statute.kjanat.dev/resolved"
)

// Severity of a lint finding.
type Severity string

// Standard severity levels. Findings of Error severity cause `-lint` to exit
// non-zero; Warnings are reported but do not fail the run.
const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Finding describes a single rule hit during a Lint pass.
type Finding struct {
	// Severity is "warning" or "error".
	Severity Severity
	// Code is the stable rule identifier (e.g. "RHT001"). Use this in
	// suppress directives once those exist.
	Code string
	// Message is a one-line human-readable description.
	Message string
	// Path is a config-pointer-style string identifying the offending
	// element (e.g. `listeners[0]`, `upstreams["api"]`).
	Path string
}

func (f Finding) String() string {
	return fmt.Sprintf("[%s] %s: %s (at %s)", f.Severity, f.Code, f.Message, f.Path)
}

// Lint validates the surface config (via Resolve) and then runs the
// production-readiness rule set against the resolved schema. Returns the
// findings in declaration order; structural validation errors come back as
// the error return.
//
// The rule set is intentionally small and stable for v0.2.0. Additional
// rules will be added in later releases.
func Lint(cfg Config) ([]Finding, error) {
	r, err := Resolve(cfg)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, rule := range lintRules {
		findings = append(findings, rule(r)...)
	}
	return findings, nil
}

// lintRules is the registry. Each function inspects the resolved config and
// returns zero or more findings.
var lintRules = []func(*resolved.Config) []Finding{
	ruleReadHeaderTimeout,
	ruleHealthCheck,
	ruleObservability,
	ruleSingleBackend,
	ruleAutoTLSStorage,
	ruleRateLimitMinimum,
	ruleBasicAuthOverHTTP,
	ruleGracePeriod,
}

func ruleReadHeaderTimeout(c *resolved.Config) []Finding {
	if c.Defaults.ReadHeaderTimeout == 0 {
		return []Finding{{
			Severity: SeverityError,
			Code:     "RHT001",
			Message:  "Defaults.ReadHeaderTimeout is zero — Slowloris vulnerability. Set to 5s or higher.",
			Path:     "defaults.read_header_timeout",
		}}
	}
	return nil
}

func ruleHealthCheck(c *resolved.Config) []Finding {
	var out []Finding
	for name, pool := range c.Upstreams {
		if !pool.HealthCheck.Enabled {
			out = append(out, Finding{
				Severity: SeverityWarning,
				Code:     "HC001",
				Message:  "Upstream pool has no active health check; dead backends will keep receiving traffic.",
				Path:     fmt.Sprintf("upstreams[%q]", name),
			})
		}
	}
	return out
}

func ruleObservability(c *resolved.Config) []Finding {
	var out []Finding
	if !c.Observability.Metrics.Enabled {
		out = append(out, Finding{
			Severity: SeverityWarning,
			Code:     "OBS001",
			Message:  "Metrics endpoint disabled; no Prometheus visibility.",
			Path:     "observability.metrics",
		})
	}
	if !c.Observability.AccessLog.Enabled {
		out = append(out, Finding{
			Severity: SeverityWarning,
			Code:     "OBS002",
			Message:  "Access log disabled; no per-request audit trail.",
			Path:     "observability.access_log",
		})
	}
	return out
}

func ruleSingleBackend(c *resolved.Config) []Finding {
	var out []Finding
	for name, pool := range c.Upstreams {
		primary := 0
		for _, b := range pool.Backends {
			if !b.Backup {
				primary++
			}
		}
		if primary == 1 {
			out = append(out, Finding{
				Severity: SeverityWarning,
				Code:     "LB001",
				Message:  "Pool has only one primary backend; no failover or load distribution. Add backends or accept the single point of failure explicitly.",
				Path:     fmt.Sprintf("upstreams[%q]", name),
			})
		}
	}
	return out
}

func ruleAutoTLSStorage(c *resolved.Config) []Finding {
	var out []Finding
	for i, l := range c.Listeners {
		if l.AutoTLS == nil {
			continue
		}
		if strings.HasPrefix(l.AutoTLS.Storage, "/tmp/") || l.AutoTLS.Storage == "/tmp" {
			out = append(out, Finding{
				Severity: SeverityError,
				Code:     "TLS001",
				Message:  "AutoTLS storage path is under /tmp; will be wiped on reboot and trigger Let's Encrypt rate-limit lockout. Use a persistent volume.",
				Path:     fmt.Sprintf("listeners[%d].auto_tls.storage", i),
			})
		}
	}
	return out
}

func ruleRateLimitMinimum(c *resolved.Config) []Finding {
	var out []Finding
	for i, route := range c.Routes {
		for j, mw := range route.Middleware {
			if mw.Type == resolved.MWRateLimit && mw.RateLimitPerSecond > 0 && mw.RateLimitPerSecond < 1 {
				out = append(out, Finding{
					Severity: SeverityWarning,
					Code:     "RL001",
					Message:  fmt.Sprintf("RateLimit is below 1/s (configured: %.3f/s); legitimate clients may be blocked.", mw.RateLimitPerSecond),
					Path:     fmt.Sprintf("routes[%d].middleware[%d]", i, j),
				})
			}
		}
	}
	return out
}

func ruleBasicAuthOverHTTP(c *resolved.Config) []Finding {
	hasHTTPSContent := false
	for _, l := range c.Listeners {
		if l.Scheme == schemeHTTPS && l.Redirect == "" {
			hasHTTPSContent = true
			break
		}
	}
	if hasHTTPSContent {
		return nil
	}
	var out []Finding
	for i, route := range c.Routes {
		for j, mw := range route.Middleware {
			if mw.Type == resolved.MWBasicAuth {
				out = append(out, Finding{
					Severity: SeverityError,
					Code:     "AUTH001",
					Message:  "BasicAuth is configured but no HTTPS listener serves content; credentials would travel in clear-text.",
					Path:     fmt.Sprintf("routes[%d].middleware[%d]", i, j),
				})
			}
		}
	}
	return out
}

func ruleGracePeriod(c *resolved.Config) []Finding {
	if c.Shutdown.GracePeriod < 5*time.Second {
		return []Finding{{
			Severity: SeverityWarning,
			Code:     "SHUT001",
			Message:  fmt.Sprintf("Shutdown.GracePeriod is %s (less than 5s); in-flight requests may be cut off during deploys.", c.Shutdown.GracePeriod),
			Path:     "shutdown.grace_period",
		}}
	}
	return nil
}
