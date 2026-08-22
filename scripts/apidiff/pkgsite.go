package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	// pkgSiteAPI is the pkg.go.dev v1 API: unauthenticated, rate limited to
	// 45 requests per second per IP. A full run makes one request per package
	// plus one to enumerate them, so the limit is never in play.
	pkgSiteAPI = "https://pkg.go.dev/v1"

	// fetchAttempts is the total number of tries per request. pkg.go.dev
	// answers 5xx while a page is being regenerated; that is worth retrying,
	// and three attempts keep a genuinely broken CI run short.
	fetchAttempts = 3
	fetchBackoff  = 500 * time.Millisecond
	fetchTimeout  = 30 * time.Second
)

// Baseline is a published surface plus the version it was published at. It is
// the on-disk format for -save and -baseline, which let the gate run with no
// network access at all.
type Baseline struct {
	ModulePath string  `json:"modulePath"`
	Version    string  `json:"version"`
	Packages   Surface `json:"packages"`
}

// client reads published surfaces from the pkg.go.dev v1 API.
type client struct {
	base string
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: base, http: &http.Client{Timeout: fetchTimeout}}
}

// symbolsResponse is the /v1/symbols/<package> payload.
type symbolsResponse struct {
	ModulePath string `json:"modulePath"`
	Version    string `json:"version"`
	Symbols    struct {
		Items         []Symbol `json:"items"`
		Total         int      `json:"total"`
		NextPageToken string   `json:"nextPageToken"`
	} `json:"symbols"`
}

// packagesResponse is the /v1/packages/<module> payload.
type packagesResponse struct {
	ModulePath string `json:"modulePath"`
	Version    string `json:"version"`
	Packages   struct {
		Items []struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	} `json:"packages"`
}

// baseline fetches the exported surface of every importable library package
// in the module's latest published version.
func (c *client) baseline(ctx context.Context, modulePath string) (*Baseline, error) {
	paths, version, err := c.packages(ctx, modulePath)
	if err != nil {
		return nil, err
	}
	base := &Baseline{ModulePath: modulePath, Version: version, Packages: Surface{}}
	for _, pkg := range paths {
		syms, err := c.symbols(ctx, pkg)
		if err != nil {
			return nil, err
		}
		base.Packages[pkg] = syms
	}
	return base, nil
}

// packages lists the module's importable library packages. Commands and
// internal packages are dropped for the same reason the local walk drops
// them: nobody can import either, so neither can be broken.
func (c *client) packages(ctx context.Context, modulePath string) ([]string, string, error) {
	var (
		paths   []string
		version string
		token   string
	)
	for {
		var resp packagesResponse
		if err := c.get(ctx, "/packages/"+modulePath, token, &resp); err != nil {
			return nil, "", err
		}
		version = resp.Version
		for _, item := range resp.Packages.Items {
			if item.Name == "main" || isInternal(item.Path) {
				continue
			}
			paths = append(paths, item.Path)
		}
		if token = resp.Packages.NextPageToken; token == "" {
			return paths, version, nil
		}
	}
}

// symbols fetches one package's published symbols, following pagination: a
// surface wider than a hundred symbols arrives in pages, and a truncated
// baseline would read as a pile of additions.
func (c *client) symbols(ctx context.Context, importPath string) ([]Symbol, error) {
	var (
		syms  []Symbol
		token string
	)
	for {
		var resp symbolsResponse
		if err := c.get(ctx, "/symbols/"+importPath, token, &resp); err != nil {
			return nil, err
		}
		syms = append(syms, resp.Symbols.Items...)
		if token = resp.Symbols.NextPageToken; token == "" {
			return syms, nil
		}
	}
}

// get performs one GET, retrying a failure a couple of times with a growing
// backoff before giving up.
func (c *client) get(ctx context.Context, path, token string, v any) error {
	endpoint := c.base + path
	if token != "" {
		endpoint += "?token=" + url.QueryEscape(token)
	}
	var err error
	for attempt := range fetchAttempts {
		if attempt > 0 && !wait(ctx, fetchBackoff<<(attempt-1)) {
			break
		}
		if err = c.fetch(ctx, endpoint, v); err == nil {
			return nil
		}
	}
	return fmt.Errorf("GET %s: %w", endpoint, err)
}

func (c *client) fetch(ctx context.Context, endpoint string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// wait sleeps for d, reporting false if the context ended first.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// loadBaseline reads a surface saved by -save.
func loadBaseline(name string) (*Baseline, error) {
	data, err := os.ReadFile(name) //nolint:gosec // G304: the path is the operator's own -baseline flag
	if err != nil {
		return nil, err
	}
	var base Baseline
	if err := json.Unmarshal(data, &base); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(base.Packages) == 0 {
		return nil, fmt.Errorf("%s: no packages in baseline; expected a file written by -save", name)
	}
	return &base, nil
}

// saveBaseline writes a fetched surface so later runs can diff offline.
func saveBaseline(name string, base *Baseline) error {
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(name, append(data, '\n'), 0o600) //nolint:gosec // G703: the path is the operator's own -save flag
}
