// Package migrate audits Traefik inputs without starting either proxy.
package migrate

import (
	"fmt"
	"io"
	"slices"
	"strings"

	statute "statute.kjanat.dev"
)

// Classification describes the disposition of a source field or object.
type Classification string

// Supported migration classifications.
const (
	Exact       Classification = "exact"
	Translated  Classification = "translated"
	Manual      Classification = "manual"
	Unsupported Classification = "unsupported"
	OutOfScope  Classification = "out_of_scope"
)

// Field intentionally contains no source value. Diagnostics must never echo input.
type Field struct {
	Path           string         `json:"path"`
	Classification Classification `json:"classification"`
	Reason         string         `json:"reason"`
	References     []string       `json:"references,omitempty"`
}

// Object retains source identity even when generation is refused.
type Object struct {
	Source         string         `json:"source"`
	Provider       string         `json:"provider"`
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	Classification Classification `json:"classification"`
	Generated      bool           `json:"generated"`
	Fields         []Field        `json:"fields"`
	data           map[string]any
	labels         map[string]string
}

// Check is a bounded diagnostic from the canonical configuration tools.
type Check struct {
	Tool     string `json:"tool"`
	Code     string `json:"code"`
	Blocking bool   `json:"blocking"`
	Deferred bool   `json:"deferred,omitempty"`
}

// Report is a versioned, deterministic audit; source values are excluded.
type Report struct {
	Version            int       `json:"version"`
	Objects            []*Object `json:"objects"`
	Checks             []Check   `json:"checks"`
	Routes             int       `json:"source_routes"`
	Hosts              int       `json:"source_hosts"`
	UnknownRouteCounts int       `json:"unknown_route_counts"`
	GeneratedRoutes    int       `json:"generated_routes"`
	Unmatched          []string  `json:"unmatched_objects"`
	Blocking           bool      `json:"blocking"`
}

// Result contains the review artifacts and the exact configuration being checked.
type Result struct {
	Report    Report
	Go        []byte
	Config    statute.Config
	inputs    Inputs
	documents map[string]string
}

// WriteHuman prints every classified field and explicitly lists omissions.
func (r Report) WriteHuman(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Traefik migration: routes=%d hosts=%d unknown-rule-counts=%d generated-routes=%d blocking=%t\n", r.Routes, r.Hosts, r.UnknownRouteCounts, r.GeneratedRoutes, r.Blocking)
	for _, o := range r.Objects {
		fmt.Fprintf(&b, "%s %s %q@%s (%q), generated=%t\n", o.Classification, o.Kind, o.Name, o.Provider, o.Source, o.Generated)
		for _, f := range o.Fields {
			fmt.Fprintf(&b, "  %s %q: %s\n", f.Classification, f.Path, f.Reason)
		}
	}
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "check %s: %s (blocking=%t, deferred=%t)\n", c.Tool, c.Code, c.Blocking, c.Deferred)
	}
	for _, u := range r.Unmatched {
		fmt.Fprintf(&b, "unmatched: %q\n", u)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func (o *Object) mark(prefix string, c Classification, reason string) {
	for i := range o.Fields {
		f := &o.Fields[i]
		path := f.Path
		if o.Provider == providerDocker {
			path = strings.ToLower(path)
			prefix = strings.ToLower(prefix)
		}
		if prefix == "" || path == prefix || strings.HasPrefix(path, prefix+".") || strings.HasPrefix(path, prefix+"[") {
			f.Classification, f.Reason = c, reason
		}
	}
}

func (o *Object) note(path string, c Classification, reason string) {
	o.Fields = append(o.Fields, Field{Path: path, Classification: c, Reason: reason})
}

func (o *Object) reference(path, kind, name string) {
	if !literal(name) {
		return
	}
	for i := range o.Fields {
		f := &o.Fields[i]
		fieldPath := f.Path
		if o.Provider == providerDocker {
			fieldPath = strings.ToLower(fieldPath)
			path = strings.ToLower(path)
		}
		if fieldPath == path || strings.HasPrefix(fieldPath, path+"[") {
			f.References = append(f.References, kind+":"+qualified(name, o.Provider))
			slices.Sort(f.References)
			f.References = slices.Compact(f.References)
		}
	}
}

func blocked(c Classification) bool { return c == Manual || c == Unsupported }

func (o *Object) ready() bool {
	for _, f := range o.Fields {
		if blocked(f.Classification) {
			return false
		}
	}
	return true
}

func (o *Object) identity() string { return o.Kind + ":" + o.Name + "@" + o.Provider }

func finish(r *Report) {
	slices.SortFunc(r.Objects, func(a, b *Object) int {
		if n := strings.Compare(a.identity(), b.identity()); n != 0 {
			return n
		}
		return strings.Compare(a.Source, b.Source)
	})
	for _, o := range r.Objects {
		o.Classification = Exact
		for _, f := range o.Fields {
			if rank(f.Classification) > rank(o.Classification) {
				o.Classification = f.Classification
			}
		}
		slices.SortFunc(o.Fields, func(a, b Field) int { return strings.Compare(a.Path, b.Path) })
		if !o.Generated {
			r.Unmatched = append(r.Unmatched, o.identity())
		}
		if blocked(o.Classification) {
			r.Blocking = true
		}
	}
	slices.Sort(r.Unmatched)
	slices.SortFunc(r.Checks, func(a, b Check) int { return strings.Compare(a.Tool+":"+a.Code, b.Tool+":"+b.Code) })
	for _, c := range r.Checks {
		if c.Blocking {
			r.Blocking = true
		}
	}
}

func rank(c Classification) int {
	switch c {
	case Exact:
		return 0
	case Translated:
		return 1
	case OutOfScope:
		return 2
	case Manual:
		return 3
	default:
		return 4
	}
}

const (
	providerDocker  = "docker"
	providerFile    = "file"
	protocolHTTP    = "http"
	kindTLS         = "tls"
	kindProviders   = "providers"
	kindEntrypoints = "entrypoints"
)

const (
	protocolHTTPS = "https"
	kindContainer = "container"
)

const (
	fieldDirectory  = "directory"
	defaultName     = "default"
	falseValue      = "false"
	removeOperation = "Remove"
)

const (
	toolBindings   = "bindings"
	toolResolve    = "resolve"
	toolExport     = "export"
	toolTopology   = "topology"
	providerStatic = "static"
	toolGraph      = "graph"
	toolLint       = "lint"
	trueValue      = "true"
)
