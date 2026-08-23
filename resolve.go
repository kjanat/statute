package statute

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"statute.kjanat.dev/internal/parse"
	"statute.kjanat.dev/resolved"
)

// Resolve validates the surface configuration, fills defaults, and produces
// the canonical resolved schema. Resolve is pure: it does not touch the
// network, the filesystem, or process state.
func Resolve(cfg Config) (*resolved.Config, error) {
	out := &resolved.Config{
		Upstreams: make(map[string]*resolved.Pool, len(cfg.Upstreams)),
	}

	defaults, err := resolveDefaults(cfg.Defaults)
	if err != nil {
		return nil, fmt.Errorf("defaults: %w", err)
	}
	out.Defaults = defaults

	if err := resolveUpstreams(cfg.Upstreams, out.Upstreams); err != nil {
		return nil, err
	}
	if err := resolveListeners(cfg.Listeners, out); err != nil {
		return nil, err
	}
	if err := resolveRoutes(cfg.Routes, out); err != nil {
		return nil, err
	}

	docker, err := resolveDocker(cfg.Docker)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	out.Docker = docker

	obs, err := resolveObservability(cfg.Observability)
	if err != nil {
		return nil, fmt.Errorf("observability: %w", err)
	}
	out.Observability = obs

	sh, err := resolveShutdown(cfg.Shutdown)
	if err != nil {
		return nil, fmt.Errorf("shutdown: %w", err)
	}
	out.Shutdown = sh

	if err := validateResolved(out); err != nil {
		return nil, err
	}
	return out, nil
}

// resolveUpstreams resolves every surface pool into out, keyed by name.
func resolveUpstreams(in map[string]Pool, out map[string]*resolved.Pool) error {
	for name, pool := range in {
		rp, err := resolvePool(name, pool)
		if err != nil {
			return fmt.Errorf("upstream %q: %w", name, err)
		}
		out[name] = rp
	}
	return nil
}

// resolveListeners resolves every surface listener and appends it to out,
// then validates the TLS decisions that only the whole set can settle.
func resolveListeners(in []*Listener, out *resolved.Config) error {
	for i, l := range in {
		rl, err := resolveListener(l)
		if err != nil {
			return fmt.Errorf("listener[%d]: %w", i, err)
		}
		out.Listeners = append(out.Listeners, rl)
	}
	return validateTLSAcrossListeners(out.Listeners)
}

// validateTLSAcrossListeners rejects ACME configurations that are only
// broken in combination: a per-listener check cannot see the plain-HTTP
// listener that serves HTTP-01 tokens, the pinned source on another
// listener persisting a domain to the same file, or the sibling source
// sharing an ACME account.
func validateTLSAcrossListeners(listeners []*resolved.Listener) error {
	if err := validateHTTP01HasPlainListener(listeners); err != nil {
		return err
	}
	if err := validatePinnedDomainCollisions(listeners); err != nil {
		return err
	}
	return validatePinnedACMEAccounts(listeners)
}

// validateHTTP01HasPlainListener rejects an HTTP-01-pinned source in a
// config with no plain HTTP listener. The in-tree manager's token table is
// layered onto "http" listeners only (server.buildListenerHandler calls
// wrapACMEChallenges under that scheme), so the validator would find
// nothing serving /.well-known/acme-challenge/* and the source burns a
// failed validation on every start. RFC 8555 §8.3 fixes http-01 to port 80
// at the validator, but the bind address is not checked here: operators
// port-map, and a listener on :8080 behind a NAT rule is a working
// deployment.
func validateHTTP01HasPlainListener(listeners []*resolved.Listener) error {
	for _, l := range listeners {
		if l.Scheme == schemeHTTP {
			return nil
		}
	}
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			if a.Challenge == resolved.ChallengeHTTP01 {
				return fmt.Errorf("auto_tls: HTTP-01 source for %s requires a plain HTTP listener to serve the challenge tokens; add statute.HTTP(\":80\") (a RedirectTo listener counts)", strings.Join(a.Domains, ", "))
			}
		}
	}
	return nil
}

// validatePinnedDomainCollisions rejects two pinned sources whose
// certificate for one domain would live at the same path. The runtime
// builds one in-tree manager per pinned source (server.initACMEManagers),
// and each persists to <storage>/<challenge>/<domain>.{crt,key} — two
// managers sharing that path race to issue and rename over each other's
// key pair. Only the file matters: the same domain on two automatic
// sources is fine (they feed one shared autocert manager, which unions
// its domains), and so are pinned sources with distinct storage roots or
// challenge kinds. Within one listener validateTLSSourceCoverage already
// rejects any duplicate name.
func validatePinnedDomainCollisions(listeners []*resolved.Listener) error {
	owner := make(map[string]*resolved.Listener)
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			if a.Challenge == resolved.ChallengeAuto {
				continue
			}
			for _, d := range a.Domains {
				path := filepath.Join(a.Storage, acmeChallengeDir(a), d)
				if prev, ok := owner[path]; ok {
					return fmt.Errorf("auto_tls: domain %q on listeners %s and %s stores its certificate at the same path (%s.crt); give the sources distinct storage roots or issue the domain from one listener", d, prev.Addr, l.Addr, path)
				}
				owner[path] = l
			}
		}
	}
	return nil
}

// validatePinnedACMEAccounts rejects pinned sources that share one ACME
// account but disagree on the contact email. A pinned source keeps its
// account key at <storage>/<challenge>/account.key (newACMEManager), so
// two sources share an account exactly when both the storage root and the
// challenge subdirectory match. x/crypto's Client.Register then returns
// ErrAccountAlreadyExists for the second source without applying its
// contact, so one of the two addresses is silently lost and which one
// depends on map iteration order. Mirrors the email agreement
// buildAutocertManager requires of the automatic sources.
func validatePinnedACMEAccounts(listeners []*resolved.Listener) error {
	first := make(map[string]*resolved.AutoTLS)
	for _, l := range listeners {
		for _, a := range l.AutoTLSSources {
			if a.Challenge == resolved.ChallengeAuto {
				continue // autocert's shared account; buildAutocertManager checks it
			}
			account := filepath.Join(a.Storage, acmeChallengeDir(a))
			prev, ok := first[account]
			if !ok {
				first[account] = a
				continue
			}
			if prev.Email != a.Email {
				return fmt.Errorf("auto_tls: email mismatch across sources sharing the ACME account at %s (%q vs %q)", account, prev.Email, a.Email)
			}
		}
	}
	return nil
}

// acmeChallengeDir names the storage subdirectory the in-tree manager
// creates for a pinned source — the names newDNS01Manager and
// newHTTP01Manager pass to newACMEManager.
func acmeChallengeDir(a *resolved.AutoTLS) string {
	if a.DNS01 != nil {
		return "dns01"
	}
	return "http01"
}

// resolveRoutes resolves every surface route and appends it to out.
// Upstream references are looked up in out.Upstreams, so resolveUpstreams
// must run first.
func resolveRoutes(in []*Route, out *resolved.Config) error {
	for i, r := range in {
		rr, err := resolveRoute(r, out.Upstreams)
		if err != nil {
			return fmt.Errorf("route[%d] %q: %w", i, r.pattern, err)
		}
		out.Routes = append(out.Routes, rr)
	}
	return nil
}

func resolveDefaults(d Defaults) (resolved.Defaults, error) {
	rhT, err := parse.DurationOr(d.ReadHeaderTimeout, 5*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("read_header_timeout: %w", err)
	}
	rT, err := parse.DurationOr(d.ReadTimeout, 0)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("read_timeout: %w", err)
	}
	wT, err := parse.DurationOr(d.WriteTimeout, 30*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("write_timeout: %w", err)
	}
	iT, err := parse.DurationOr(d.IdleTimeout, 120*time.Second)
	if err != nil {
		return resolved.Defaults{}, fmt.Errorf("idle_timeout: %w", err)
	}
	mhb := d.MaxHeaderBytes
	if mhb == 0 {
		mhb = 1 << 20 // 1MB, matches Go's default
	}
	return resolved.Defaults{
		ReadHeaderTimeout: rhT,
		ReadTimeout:       rT,
		WriteTimeout:      wT,
		IdleTimeout:       iT,
		MaxHeaderBytes:    mhb,
	}, nil
}

func resolvePool(name string, p Pool) (*resolved.Pool, error) {
	if len(p.Backends) == 0 {
		return nil, errors.New("pool has no backends")
	}
	rp := &resolved.Pool{
		Name:     name,
		Strategy: resolved.Strategy(p.Strategy),
	}
	for i, b := range p.Backends {
		if strings.TrimSpace(b.Address) == "" {
			return nil, fmt.Errorf("backend[%d]: address is empty", i)
		}
		w := b.Weight
		if w == 0 {
			w = 1
		}
		rp.Backends = append(rp.Backends, resolved.Backend{
			Address: b.Address,
			Weight:  w,
			Backup:  b.Backup,
		})
	}
	hc, err := resolveHealthCheck(p.HealthCheck)
	if err != nil {
		return nil, fmt.Errorf("health_check: %w", err)
	}
	rp.HealthCheck = hc
	tr, err := resolveTransport(p.Transport)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	rp.Transport = tr
	if err := resolveUpstreamHost(p.UpstreamHost, rp); err != nil {
		return nil, fmt.Errorf("upstream_host: %w", err)
	}
	return rp, nil
}

// resolveUpstreamHost maps the surface Host policy onto the resolved pool.
// An explicit value is a header value bound for the wire, so it gets the
// header-injection validation configured headers get, plus a token check
// light enough to admit any real host:port.
func resolveUpstreamHost(u UpstreamHost, rp *resolved.Pool) error {
	switch u.mode {
	case hostModeClient:
		rp.UpstreamHost = resolved.HostClient
	case hostModeTarget:
		rp.UpstreamHost = resolved.HostTarget
	case hostModeExplicit:
		if strings.TrimSpace(u.value) == "" {
			return errors.New("HostValue is empty")
		}
		if _, err := parse.HeaderValue(u.value); err != nil {
			return err
		}
		rp.UpstreamHost = resolved.HostExplicit
		rp.HostValue = u.value
	}
	return nil
}

func resolveHealthCheck(h HealthCheck) (resolved.HealthCheck, error) {
	if h.Path == "" {
		return resolved.HealthCheck{Enabled: false}, nil
	}
	interval, err := parse.DurationOr(h.Interval, 10*time.Second)
	if err != nil {
		return resolved.HealthCheck{}, fmt.Errorf("interval: %w", err)
	}
	timeout, err := parse.DurationOr(h.Timeout, 2*time.Second)
	if err != nil {
		return resolved.HealthCheck{}, fmt.Errorf("timeout: %w", err)
	}
	healthy := h.Healthy
	if healthy == 0 {
		healthy = 2
	}
	unhealthy := h.Unhealthy
	if unhealthy == 0 {
		unhealthy = 3
	}
	return resolved.HealthCheck{
		Enabled:   true,
		Path:      h.Path,
		Interval:  interval,
		Timeout:   timeout,
		Healthy:   healthy,
		Unhealthy: unhealthy,
	}, nil
}

func resolveTransport(t Transport) (resolved.Transport, error) {
	maxIdle := t.MaxIdleConnsPerHost
	if maxIdle == 0 {
		maxIdle = 32
	}
	idle, err := parse.DurationOr(t.IdleConnTimeout, 90*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("idle_conn_timeout: %w", err)
	}
	dial, err := parse.DurationOr(t.DialTimeout, 5*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("dial_timeout: %w", err)
	}
	tlsHs, err := parse.DurationOr(t.TLSHandshakeTimeout, 5*time.Second)
	if err != nil {
		return resolved.Transport{}, fmt.Errorf("tls_handshake_timeout: %w", err)
	}
	for i, f := range t.RootCAFiles {
		if strings.TrimSpace(f) == "" {
			return resolved.Transport{}, fmt.Errorf("root_ca_files[%d]: path is empty", i)
		}
	}
	return resolved.Transport{
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     idle,
		DialTimeout:         dial,
		TLSHandshakeTimeout: tlsHs,
		ServerName:          t.ServerName,
		RootCAFiles:         append([]string(nil), t.RootCAFiles...),
		InsecureSkipVerify:  t.InsecureSkipVerify,
	}, nil
}

// resolveDocker fills provider defaults and parses the refresh interval.
// The endpoint's scheme is validated here so a typo fails at Resolve time,
// not when the runtime first dials the daemon.
func resolveDocker(d *DockerConfig) (*resolved.Docker, error) {
	if d == nil {
		return nil, nil
	}
	endpoint := d.endpoint
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	if !strings.HasPrefix(endpoint, "unix://") && !strings.HasPrefix(endpoint, "tcp://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("endpoint %q: must be unix:// or tcp://", endpoint)
	}
	refresh, err := parse.DurationOr(d.refresh, 0)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	registry, defaults, err := resolveDockerMiddleware(d)
	if err != nil {
		return nil, err
	}
	return &resolved.Docker{
		Endpoint:          endpoint,
		Network:           d.network,
		ExposedByDefault:  d.exposedByDefault,
		TraefikLabels:     d.traefikLabels,
		Refresh:           refresh,
		Middleware:        registry,
		DefaultMiddleware: defaults,
	}, nil
}

// resolveDockerMiddleware resolves the code-owned registry of named
// middleware chains and the provider-wide default chain. Both stay nil
// when undeclared.
func resolveDockerMiddleware(d *DockerConfig) (map[string][]resolved.Middleware, []resolved.Middleware, error) {
	var registry map[string][]resolved.Middleware
	for _, name := range slices.Sorted(maps.Keys(d.middleware)) {
		mws, err := resolveMiddlewares(d.middleware[name])
		if err != nil {
			return nil, nil, fmt.Errorf("middleware %q: %w", name, err)
		}
		if registry == nil {
			registry = make(map[string][]resolved.Middleware, len(d.middleware))
		}
		registry[name] = mws
	}
	defaults, err := resolveMiddlewares(d.defaultMiddleware)
	if err != nil {
		return nil, nil, fmt.Errorf("default middleware: %w", err)
	}
	return registry, defaults, nil
}

func resolveListener(l *Listener) (*resolved.Listener, error) {
	if l == nil {
		return nil, errors.New("nil listener")
	}
	if strings.TrimSpace(l.addr) == "" {
		return nil, errors.New("address is empty")
	}
	rl := &resolved.Listener{
		Addr:             l.addr,
		Scheme:           l.scheme,
		Redirect:         l.redirect,
		EnableHTTP2:      l.enableHTTP2,
		HTTP3Addr:        l.http3Addr,
		BehindCloudflare: l.behindCF,
	}
	if err := resolveListenerTLS(l, rl); err != nil {
		return nil, err
	}
	if err := resolveTLSPolicy(l, rl); err != nil {
		return nil, err
	}
	if err := resolveTrustedProxy(l.trustedProxy, rl); err != nil {
		return nil, err
	}
	return rl, nil
}

// resolveTrustedProxy canonicalises the listener's peer-trust declaration:
// the CIDRs every trusted peer must fall inside, and the forwarded header —
// X-Forwarded-For unless configured — consulted when one does.
func resolveTrustedProxy(t *TrustedProxyConfig, rl *resolved.Listener) error {
	if t == nil {
		return nil
	}
	if len(t.cidrs) == 0 {
		return errors.New("trusted_proxy: at least one CIDR required")
	}
	canon, err := resolveCIDRs(t.cidrs)
	if err != nil {
		return fmt.Errorf("trusted_proxy: %w", err)
	}
	rl.TrustedProxies = canon
	// No empty-name fallback here: TrustedProxy() seeds the default, so an
	// empty name at this point was configured explicitly — likely a value
	// that went missing — and silently trusting X-Forwarded-For instead
	// would change the security policy. parse.HeaderName rejects it.
	name, err := parse.HeaderName(t.header)
	if err != nil {
		return fmt.Errorf("trusted_proxy: %w", err)
	}
	rl.ClientIPHeader = name
	return nil
}

// validateListenerTLSPresence rejects an HTTPS content listener that carries
// no TLS material. Redirect-only listeners and plain HTTP need none.
func validateListenerTLSPresence(l *Listener) error {
	if l.scheme == schemeHTTPS && l.redirect == "" && len(l.autoTLS) == 0 && len(l.staticTLS) == 0 {
		return errors.New("https listener requires AutoTLS or StaticTLS")
	}
	return nil
}

// resolveListenerTLS lowers every TLS source declared on the listener into
// the resolved source slices, validates the combination, and mirrors the
// first source of each kind into the legacy singular fields.
func resolveListenerTLS(l *Listener, rl *resolved.Listener) error {
	if err := validateListenerTLSPresence(l); err != nil {
		return err
	}
	for _, a := range l.autoTLS {
		at, err := resolveAutoTLS(a)
		if err != nil {
			return err
		}
		rl.AutoTLSSources = append(rl.AutoTLSSources, at)
	}
	for _, s := range l.staticTLS {
		st, err := resolveStaticTLS(s)
		if err != nil {
			return err
		}
		rl.StaticTLSSources = append(rl.StaticTLSSources, st)
	}
	if err := validateTLSSourceCoverage(rl); err != nil {
		return err
	}
	if len(rl.AutoTLSSources) > 0 {
		rl.AutoTLS = rl.AutoTLSSources[0]
	}
	if len(rl.StaticTLSSources) > 0 {
		rl.StaticTLS = rl.StaticTLSSources[0]
	}
	return nil
}

// validateTLSSourceCoverage rejects ambiguous SNI coverage on one listener:
// a name (exact or wildcard pattern) claimed by two sources would make the
// handshake-time choice arbitrary, and a second hostless static fallback
// could never be selected. An exact name overlapping a wildcard from
// another source is fine — the exact name wins at handshake time.
func validateTLSSourceCoverage(rl *resolved.Listener) error {
	// Sources arrive with canonical names (canonicalTLSName), so claims
	// compare byte-for-byte.
	claimed := make(map[string]string)
	claim := func(name, source string) error {
		if prev, ok := claimed[name]; ok {
			return fmt.Errorf("tls: %s and %s both claim %q; each SNI name may have one source per listener", prev, source, name)
		}
		claimed[name] = source
		return nil
	}
	for i, a := range rl.AutoTLSSources {
		for _, d := range a.Domains {
			if err := claim(d, fmt.Sprintf("auto_tls[%d]", i)); err != nil {
				return err
			}
		}
	}
	fallbackSeen := false
	for i, st := range rl.StaticTLSSources {
		if st.Host == "" {
			if fallbackSeen {
				return errors.New("static_tls: only one hostless fallback source per listener; scope the others with StaticTLSFor")
			}
			fallbackSeen = true
			continue
		}
		if err := claim(st.Host, fmt.Sprintf("static_tls[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// resolveAutoTLS validates an AutoTLS surface config (domains, email,
// storage, optional Cloudflare DNS-01) and produces its resolved form.
func resolveAutoTLS(a *AutoTLSConfig) (*resolved.AutoTLS, error) {
	if len(a.Domains) == 0 {
		return nil, errors.New("auto_tls: at least one domain required")
	}
	if a.email == "" {
		return nil, errors.New("auto_tls: email required for ACME registration")
	}
	if a.storage == "" {
		return nil, errors.New("auto_tls: storage path required for cert persistence")
	}
	if err := validateAutoTLSChallenge(a); err != nil {
		return nil, err
	}
	domains, err := canonicalAutoTLSDomains(a.Domains)
	if err != nil {
		return nil, err
	}
	at := &resolved.AutoTLS{
		Domains: domains,
		Email:   a.email,
		Storage: a.storage,
	}
	switch {
	case a.dns01 != nil:
		dns01, derr := resolveCloudflareDNS01(a)
		if derr != nil {
			return nil, derr
		}
		at.DNS01 = dns01
		at.Challenge = resolved.ChallengeDNS01
	case a.explicitHTTP01:
		at.Challenge = resolved.ChallengeHTTP01
	}
	return at, nil
}

// DNS-01 propagation bounds. The maxima are absurdity guards, not tuning
// knobs: a policy that waits longer than ten minutes for one record has a
// broken zone, not a slow one, and every minute of it is spent inside the
// order the CA is holding open.
const (
	dnsPropagationMaxWait         = 10 * time.Minute
	dnsPropagationDefaultTimeout  = 2 * time.Minute
	dnsPropagationDefaultInterval = 5 * time.Second
	dnsPropagationMinInterval     = 100 * time.Millisecond
)

// resolveCloudflareDNS01 validates the Cloudflare DNS-01 settings of one
// AutoTLS source and produces their resolved form, propagation policy
// included.
func resolveCloudflareDNS01(a *AutoTLSConfig) (*resolved.CloudflareDNS01, error) {
	if a.dns01.apiToken == "" {
		return nil, errors.New("auto_tls.cloudflare_dns01: api token is required")
	}
	prop, err := resolveDNSPropagation(a.propagation)
	if err != nil {
		return nil, err
	}
	return &resolved.CloudflareDNS01{
		APIToken:    a.dns01.apiToken,
		ZoneID:      a.dns01.zoneID,
		Propagation: prop,
	}, nil
}

// resolveDNSPropagation normalises a declared propagation policy: it
// parses the duration strings, fills the polling defaults, and validates
// the resolver list. A nil policy resolves to nil — the runtime's fixed
// default wait.
func resolveDNSPropagation(p *DNSPropagation) (*resolved.DNSPropagation, error) {
	if p == nil {
		return nil, nil
	}
	delay, err := resolveDNSPropagationDelay(p.Delay)
	if err != nil {
		return nil, err
	}
	resolvers, err := resolveDNSPropagationResolvers(p.Resolvers)
	if err != nil {
		return nil, err
	}
	timeout, interval, err := resolveDNSPropagationPolling(p, len(resolvers) > 0)
	if err != nil {
		return nil, err
	}
	// Judged on the parsed value, not the spelling: Delay "0s" is as
	// empty as "". A policy that waits for nothing would silently drop
	// the built-in default wait — which is what leaving the option off
	// already expresses. Checked after the polling validation so a
	// timeout or interval without resolvers gets its own message, not a
	// claim that the policy is empty.
	if delay == 0 && len(resolvers) == 0 {
		return nil, errors.New("dns_propagation: the policy waits for nothing; set a positive delay, resolvers, or drop the option")
	}
	return &resolved.DNSPropagation{
		Delay:     delay,
		Timeout:   timeout,
		Interval:  interval,
		Resolvers: resolvers,
	}, nil
}

// resolveDNSPropagationDelay parses the fixed pre-validation wait. An
// empty delay is none at all; parse.Duration already rejects a negative
// one.
func resolveDNSPropagationDelay(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := parse.Duration(s)
	if err != nil {
		return 0, fmt.Errorf("dns_propagation: delay: %w", err)
	}
	if d > dnsPropagationMaxWait {
		return 0, fmt.Errorf("dns_propagation: delay %s is above the %s maximum; a record that slow is a broken zone, not a propagating one", d, dnsPropagationMaxWait)
	}
	return d, nil
}

// resolveDNSPropagationPolling fills the polling window. Timeout and
// Interval govern resolver verification alone, so declaring either without
// resolvers is an error rather than a setting nothing ever reads.
func resolveDNSPropagationPolling(p *DNSPropagation, hasResolvers bool) (time.Duration, time.Duration, error) {
	if !hasResolvers {
		if p.Timeout != "" || p.Interval != "" {
			return 0, 0, errors.New("dns_propagation: timeout and interval govern resolver polling, and no resolvers are set; add resolvers or drop them")
		}
		return 0, 0, nil
	}
	timeout, err := parse.DurationOr(p.Timeout, dnsPropagationDefaultTimeout)
	if err != nil {
		return 0, 0, fmt.Errorf("dns_propagation: timeout: %w", err)
	}
	if timeout == 0 {
		return 0, 0, errors.New("dns_propagation: timeout must be greater than zero; polling needs a deadline to fail at")
	}
	if timeout > dnsPropagationMaxWait {
		return 0, 0, fmt.Errorf("dns_propagation: timeout %s is above the %s maximum; a record that slow is a broken zone, not a propagating one", timeout, dnsPropagationMaxWait)
	}
	interval, err := resolveDNSPropagationInterval(p.Interval, timeout)
	if err != nil {
		return 0, 0, err
	}
	return timeout, interval, nil
}

// resolveDNSPropagationInterval parses the polling cadence against the
// window it has to fit in. The floor keeps a misconfigured policy from
// hammering the resolvers; the ceiling keeps a cadence that could never
// fire twice from masquerading as polling.
func resolveDNSPropagationInterval(s string, timeout time.Duration) (time.Duration, error) {
	if s == "" {
		// The default cadence, clamped into the window: a user who set
		// only a short timeout asked for fail-fast polling, not an error
		// about a field they never wrote.
		return min(dnsPropagationDefaultInterval, timeout), nil
	}
	interval, err := parse.Duration(s)
	if err != nil {
		return 0, fmt.Errorf("dns_propagation: interval: %w", err)
	}
	if interval < dnsPropagationMinInterval {
		return 0, fmt.Errorf("dns_propagation: interval %s is below the %s minimum; a tighter loop only floods the resolvers", interval, dnsPropagationMinInterval)
	}
	if interval > timeout {
		return 0, fmt.Errorf("dns_propagation: interval %s is above timeout %s; the loop would never poll twice", interval, timeout)
	}
	return interval, nil
}

// resolveDNSPropagationResolvers validates each "host:port" resolver and
// returns the list in declaration order, each in its canonical spelling.
// Duplicate detection runs on the canonical form, so one server listed
// twice under different spellings is caught too.
func resolveDNSPropagationResolvers(list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(list))
	for _, r := range list {
		canon, err := canonicalDNSResolverAddr(r)
		if err != nil {
			return nil, err
		}
		if slices.Contains(out, canon) {
			return nil, fmt.Errorf("dns_propagation: resolver %q is listed twice (canonical form %s)", r, canon)
		}
		out = append(out, canon)
	}
	return out, nil
}

// canonicalDNSResolverAddr validates one resolver address and returns its
// canonical "host:port" spelling: an IP literal in its canonical text
// form (so "[2001:0db8::0:1]:53" and "[2001:db8::1]:53" are one
// resolver), a hostname lowercased (DNS names are case-insensitive), the
// port in plain decimal, an IPv6 host bracketed. The canonical form is
// what the resolved schema stores and what the runtime dials. The port is
// mandatory: net.Dial has no default for DNS, so a bare address would
// fail at issuance time instead of here.
func canonicalDNSResolverAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("dns_propagation: resolver %q must be host:port, as in \"192.0.2.53:53\" or \"[2001:db8::1]:53\": %w", addr, err)
	}
	if host == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, " \t") {
		return "", fmt.Errorf("dns_propagation: resolver %q has no usable host", addr)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	} else {
		host = strings.ToLower(host)
	}
	n, ok := dnsResolverPort(port)
	if !ok {
		return "", fmt.Errorf("dns_propagation: resolver %q has port %q; want a number from 1 to 65535", addr, port)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// dnsResolverPort parses a decimal port. Digits only: strconv.Atoi would
// also take "+53", which passes resolve but fails net.Dial's own port
// parser at issuance time.
func dnsResolverPort(port string) (int, bool) {
	for _, c := range port {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// canonicalAutoTLSDomains lowers every configured ACME domain to its
// canonical routing form (case, trailing dots, IDNA A-label), rejecting
// names that canonicalise to nothing, unroutable wildcards, and — unlike a
// static host — names the IDNA lookup profile rejects. canonicalTLSName's
// lowercase fallback exists for unusual-but-working static hostnames; an
// ACME domain that needs it cannot be issued at all: autocert.HostWhitelist
// keeps only the names idna.Lookup.ToASCII accepts and drops the rest, so
// the SNI is refused at handshake time, and a pinned source submits an
// order the CA rejects on every attempt.
func canonicalAutoTLSDomains(domains []string) ([]string, error) {
	canon := make([]string, len(domains))
	for i, d := range domains {
		cd, err := canonicalTLSNameStrict(d)
		if cd == "" {
			return nil, fmt.Errorf("auto_tls: invalid domain %q", d)
		}
		if werr := validateTLSWildcard(cd); werr != nil {
			return nil, fmt.Errorf("auto_tls: invalid domain %q: %w", d, werr)
		}
		if err != nil {
			return nil, fmt.Errorf("auto_tls: domain %q is not a valid ACME identifier: %w", d, err)
		}
		canon[i] = cd
	}
	return canon, nil
}

// validateTLSWildcard rejects a canonical name whose "*" cannot route. A
// bare "*" (which "*." also canonicalises to, the trailing dot being
// stripped first) is not a name any ClientHello can send, so indexing it
// leaves the operator's intended catch-all dead — StaticTLS, the hostless
// fallback, is the way to serve unmatched names. Any other "*" — an inner
// label as in "*.*.example.com", or one inside a label — fails the IDNA
// lookup and would otherwise survive on the lowercase fallback.
func validateTLSWildcard(canonical string) error {
	if canonical == "*" || canonical == "*." {
		return errors.New(`"*" matches no SNI name`)
	}
	if strings.Contains(strings.TrimPrefix(canonical, "*."), "*") {
		return errors.New(`"*" is only allowed as a single leading "*." label`)
	}
	return nil
}

// validateAutoTLSChallenge rejects contradictory or unsatisfiable challenge
// policy on one ACME source: HTTP01 and CloudflareDNS01 cannot both be
// declared, a propagation policy needs a DNS-01 source to govern, and a
// wildcard domain can only be issued over DNS-01.
func validateAutoTLSChallenge(a *AutoTLSConfig) error {
	if a.explicitHTTP01 && a.dns01 != nil {
		return errors.New("auto_tls: HTTP01 and CloudflareDNS01 are mutually exclusive on one source")
	}
	if a.propagation != nil && a.dns01 == nil {
		return errors.New("dns_propagation: only a CloudflareDNS01 source publishes a DNS record to wait for; add CloudflareDNS01 or drop Propagation")
	}
	if a.dns01 == nil {
		for _, d := range a.Domains {
			if strings.HasPrefix(strings.TrimSpace(d), "*.") {
				return fmt.Errorf("auto_tls: wildcard domain %q requires CloudflareDNS01; HTTP-01 cannot issue wildcard certificates", d)
			}
		}
	}
	return nil
}

// resolveStaticTLS validates that both cert and key paths are set and
// produces the resolved StaticTLS form. The SNI host is canonicalised via
// canonicalTLSName — the lenient variant, so a hostname the IDNA lookup
// profile rejects still routes — so it compares equal to AutoTLS domains
// and ClientHello names; a StaticTLSFor call whose host is empty or an
// unroutable wildcard is rejected rather than silently becoming, or
// shadowing, the fallback source.
func resolveStaticTLS(s *StaticTLSConfig) (*resolved.StaticTLS, error) {
	if s.CertFile == "" || s.KeyFile == "" {
		return nil, errors.New("static_tls: cert_file and key_file required")
	}
	host := canonicalTLSName(s.Host)
	if s.hostSet {
		if host == "" {
			return nil, errors.New("static_tls: host required; use StaticTLS for the hostless fallback")
		}
		if err := validateTLSWildcard(host); err != nil {
			return nil, fmt.Errorf("static_tls: host %q: %w; use StaticTLS for the hostless fallback", s.Host, err)
		}
	}
	return &resolved.StaticTLS{
		CertFile: s.CertFile,
		KeyFile:  s.KeyFile,
		Host:     host,
	}, nil
}

func resolveRoute(r *Route, pools map[string]*resolved.Pool) (*resolved.Route, error) {
	if r == nil {
		return nil, errors.New("nil route")
	}
	if r.pattern == "" {
		return nil, errors.New("pattern is empty")
	}
	rr := &resolved.Route{
		Pattern:   r.pattern,
		Host:      r.host,
		StaticDir: r.staticDir,
	}
	if r.clientIPsSet {
		// An explicitly empty matcher would silently match every client —
		// the constraint the caller reached for would just vanish.
		if len(r.clientIPs) == 0 {
			return nil, errors.New("client_ips: at least one CIDR required")
		}
		canon, err := resolveCIDRs(r.clientIPs)
		if err != nil {
			return nil, fmt.Errorf("client_ips: %w", err)
		}
		rr.ClientIPCIDRs = canon
	}
	if err := resolveRouteTarget(r, pools, rr); err != nil {
		return nil, err
	}
	mws, err := resolveMiddlewares(r.middleware)
	if err != nil {
		return nil, err
	}
	rr.Middleware = mws
	return rr, nil
}

// resolveRouteTarget validates that the route declares exactly one action —
// ProxyTo, Serve, or RedirectTo — and resolves it: a proxy route
// dereferences the named upstream into rr.Upstream, a redirect route
// validates its target and status into rr.Redirect.
func resolveRouteTarget(r *Route, pools map[string]*resolved.Pool, rr *resolved.Route) error {
	// A RedirectTo call with an empty target still declares the redirect
	// action — the status betrays it — so it fails on its own emptiness
	// rather than as an action-less route.
	redirected := r.redirectTo != "" || r.redirectStatus != 0
	declared := 0
	for _, set := range []bool{r.upstream != "", r.staticDir != "", redirected} {
		if set {
			declared++
		}
	}
	switch {
	case declared > 1:
		return errors.New("route declares more than one of ProxyTo, Serve, and RedirectTo; pick one")
	case declared == 0:
		return errors.New("route has none of ProxyTo, Serve, or RedirectTo")
	case r.upstream != "":
		pool, ok := pools[r.upstream]
		if !ok {
			return fmt.Errorf("unknown upstream %q", r.upstream)
		}
		rr.Upstream = pool
	case redirected:
		rd, err := resolveRedirect(r.redirectTo, r.redirectStatus)
		if err != nil {
			return err
		}
		rr.Redirect = rd
	}
	return nil
}

// resolveMiddlewares resolves a route's middleware list in order,
// returning nil for an empty list so an unmiddlewared route stays nil.
func resolveMiddlewares(mws []Middleware) ([]resolved.Middleware, error) {
	if len(mws) == 0 {
		return nil, nil
	}
	out := make([]resolved.Middleware, 0, len(mws))
	for i, mw := range mws {
		rmw, err := resolveMiddleware(mw)
		if err != nil {
			return nil, fmt.Errorf("middleware[%d]: %w", i, err)
		}
		out = append(out, rmw)
	}
	return out, nil
}

// Every surface middleware implements one of these resolver interfaces. Types
// that cannot fail avoid manufacturing an error result solely for dispatch.
type fallibleMiddleware interface {
	resolve() (resolved.Middleware, error)
}

type infallibleMiddleware interface {
	resolve() resolved.Middleware
}

func resolveMiddleware(mw Middleware) (resolved.Middleware, error) {
	switch rm := mw.(type) {
	case fallibleMiddleware:
		return rm.resolve()
	case infallibleMiddleware:
		return rm.resolve(), nil
	default:
		return resolved.Middleware{}, fmt.Errorf("unknown middleware type %T", mw)
	}
}

func (m *timeoutMW) resolve() (resolved.Middleware, error)   { return resolveTimeoutMW(m) }
func (m *rateLimitMW) resolve() (resolved.Middleware, error) { return resolveRateLimitMW(m) }
func (m *retryMW) resolve() (resolved.Middleware, error)     { return resolveRetryMW(m) }
func (m *cacheMW) resolve() (resolved.Middleware, error)     { return resolveCacheMW(m) }

func (m *compressMW) resolve() resolved.Middleware { return resolveCompressMW(m) }

func (*etagMW) resolve() resolved.Middleware {
	return resolved.Middleware{Type: resolved.MWETag}
}
func (m *bodyLimitMW) resolve() (resolved.Middleware, error) { return resolveBodyLimitMW(m) }

func (m *requestIDMW) resolve() resolved.Middleware {
	return resolved.Middleware{
		Type:                resolved.MWRequestID,
		RequestIDHeader:     m.header,
		RequestIDFromHeader: m.fromHeader,
	}
}

func (m *securityHeadersMW) resolve() (resolved.Middleware, error) {
	return resolveSecurityHeadersMW(m)
}
func (m *allowIPsMW) resolve() (resolved.Middleware, error)  { return resolveAllowIPsMW(m) }
func (m *denyIPsMW) resolve() (resolved.Middleware, error)   { return resolveDenyIPsMW(m) }
func (m *basicAuthMW) resolve() (resolved.Middleware, error) { return resolveBasicAuthMW(m) }
func (m *corsMW) resolve() (resolved.Middleware, error)      { return resolveCORSMW(m) }
func (m *headerMW) resolve() (resolved.Middleware, error)    { return resolveHeaderMW(m) }

func (m *stripPrefixMW) resolve() (resolved.Middleware, error) { return resolveStripPrefixMW(m) }
func (m *addPrefixMW) resolve() (resolved.Middleware, error)   { return resolveAddPrefixMW(m) }
func (m *replacePathMW) resolve() (resolved.Middleware, error) { return resolveReplacePathMW(m) }
func (m *rewritePathMW) resolve() (resolved.Middleware, error) { return resolveRewritePathMW(m) }

// resolveTimeoutMW parses the timeout duration string.
func resolveTimeoutMW(m *timeoutMW) (resolved.Middleware, error) {
	d, err := parse.Duration(m.dur)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("timeout: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWTimeout, Timeout: d}, nil
}

// resolveRateLimitMW parses the rate string into requests/second and
// carries the rate-limit key.
func resolveRateLimitMW(m *rateLimitMW) (resolved.Middleware, error) {
	rate, err := parse.Rate(m.rate)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("rate_limit: %w", err)
	}
	return resolved.Middleware{
		Type:               resolved.MWRateLimit,
		RateLimitPerSecond: rate,
		RateLimitKey:       resolved.RateLimitKey(m.key),
	}, nil
}

// resolveRetryMW validates that max attempts is >= 1 and copies the
// retry-on-status list.
func resolveRetryMW(m *retryMW) (resolved.Middleware, error) {
	if m.max < 1 {
		return resolved.Middleware{}, errors.New("retry: max must be >= 1")
	}
	return resolved.Middleware{
		Type:            resolved.MWRetry,
		RetryMax:        m.max,
		RetryOnStatuses: append([]int(nil), m.onStatuses...),
	}, nil
}

// resolveCacheMW parses the cache TTL duration string.
func resolveCacheMW(m *cacheMW) (resolved.Middleware, error) {
	d, err := parse.Duration(m.ttl)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("cache: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWCache, CacheTTL: d}, nil
}

// resolveCompressMW maps the requested compression algorithms to their
// resolved form. It cannot fail.
func resolveCompressMW(m *compressMW) resolved.Middleware {
	algos := make([]resolved.CompressAlgo, 0, len(m.algos))
	for _, a := range m.algos {
		algos = append(algos, resolved.CompressAlgo(a))
	}
	return resolved.Middleware{Type: resolved.MWCompress, CompressAlgos: algos}
}

// resolveBodyLimitMW parses the size string and requires a positive limit.
func resolveBodyLimitMW(m *bodyLimitMW) (resolved.Middleware, error) {
	n, err := parse.Size(m.size)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("body_limit: %w", err)
	}
	if n <= 0 {
		return resolved.Middleware{}, errors.New("body_limit: size must be positive")
	}
	return resolved.Middleware{Type: resolved.MWBodyLimit, BodyLimitBytes: n}, nil
}

// resolveSecurityHeadersMW formats the optional HSTS duration and copies
// the remaining header policy values.
func resolveSecurityHeadersMW(m *securityHeadersMW) (resolved.Middleware, error) {
	hstsHeader := ""
	if m.hsts != "" {
		d, err := parse.Duration(m.hsts)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("security_headers.hsts: %w", err)
		}
		hstsHeader = formatHSTS(d)
	}
	return resolved.Middleware{
		Type:                  resolved.MWSecurityHeaders,
		SecHSTS:               hstsHeader,
		SecCSP:                m.csp,
		SecFrameOptions:       m.frameOptions,
		SecContentTypeOptions: m.contentTypeOptions,
		SecReferrerPolicy:     m.referrerPolicy,
		SecPermissionsPolicy:  m.permissionsPolicy,
	}, nil
}

// unsettableRequestHeaders are the request fields Go carries outside the
// header map, where a mutation would be a silent no-op: net/http writes them
// from Request.Host, Request.ContentLength, Request.TransferEncoding, and
// Request.Trailer and excludes the header-map entries when it writes the
// request. Rejecting them at resolve time turns a configuration that cannot
// work into a startup error.
var unsettableRequestHeaders = map[string]string{
	"Host":              "Go keeps the request authority in Request.Host",
	"Content-Length":    "Go frames the body from Request.ContentLength",
	"Transfer-Encoding": "Go frames the body from Request.TransferEncoding",
	"Trailer":           "Go writes the trailer names from Request.Trailer",
}

// resolveHeaderMW validates one header mutation and canonicalises its name.
func resolveHeaderMW(m *headerMW) (resolved.Middleware, error) {
	label := headerMWLabel(m.op)
	name, err := parse.HeaderName(m.name)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("%s: %w", label, err)
	}
	value, err := parse.HeaderValue(m.value)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("%s: %w", label, err)
	}
	if reason, ok := unsettableRequestHeaders[name]; ok && isRequestHeaderOp(m.op) {
		return resolved.Middleware{}, fmt.Errorf("%s: %q cannot be rewritten on a request; %s", label, name, reason)
	}
	return resolved.Middleware{Type: m.op, HeaderName: name, HeaderValue: value}, nil
}

// normalizePathPrefix validates and normalises a strip/add prefix: trailing
// slashes are trimmed, so "/api/" and "/api" are one declaration, and what
// survives has to be a rooted path naming at least one segment. A "?" or "#"
// belongs to the query or fragment, neither of which a prefix operation can
// touch, so carrying one is a configuration mistake rather than a literal.
//
// A "%" is rejected too: a prefix is a decoded literal that is matched against
// the decoded request path and prepended to it. A percent-escape has no
// consistent meaning in that role — StripPrefix would compare the literal
// "%2F" against a decoded "/" and never match, and AddPrefix would re-escape
// the "%" and send "%252F" upstream — so it is a mistake rather than a value.
// ReplacePath, which does operate on an escaped target, is the primitive for
// escaped paths.
func normalizePathPrefix(prefix string) (string, error) {
	if strings.ContainsAny(prefix, "?#%") {
		return "", fmt.Errorf("prefix %q must not contain %q, %q, or %q", prefix, "?", "#", "%")
	}
	p := strings.TrimRight(prefix, "/")
	if !strings.HasPrefix(p, "/") || len(p) < 2 {
		return "", fmt.Errorf("prefix %q must start with %q and name at least one path segment", prefix, "/")
	}
	if p[1] == '/' || p[1] == '\\' {
		return "", fmt.Errorf("prefix %q must not start with %q or %q: a doubled leading slash is a protocol-relative URL, not a path", prefix, "//", "/\\")
	}
	return p, nil
}

// resolveStripPrefixMW normalises the prefix to strip.
func resolveStripPrefixMW(m *stripPrefixMW) (resolved.Middleware, error) {
	p, err := normalizePathPrefix(m.prefix)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("strip_prefix: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWStripPrefix, PathPrefix: p}, nil
}

// resolveAddPrefixMW normalises the prefix to prepend.
func resolveAddPrefixMW(m *addPrefixMW) (resolved.Middleware, error) {
	p, err := normalizePathPrefix(m.prefix)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("add_prefix: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWAddPrefix, PathPrefix: p}, nil
}

// splitReplacePath validates a ReplacePath target and splits it at the first
// "?" into the path and the explicit query. querySet distinguishes a target
// that clears the query ("/x?") from one that leaves it alone ("/x"). The
// path is returned in the escaped form it was written in, so an operator who
// spelled "%2F" gets a literal slash inside a segment rather than a separator.
//
// The query half is checked too: it is written straight onto the outgoing
// request-target, so a space or a control byte there is not a value but a
// malformed request the upstream answers with 400, or a CR/LF the transport
// refuses outright. Rejecting them here makes such a target a startup error,
// the way a bad header value is, rather than a per-request failure.
func splitReplacePath(target string) (string, string, bool, error) {
	if strings.Contains(target, "#") {
		return "", "", false, fmt.Errorf("target %q must not contain %q", target, "#")
	}
	path, query, querySet := strings.Cut(target, "?")
	if !strings.HasPrefix(path, "/") {
		return "", "", false, fmt.Errorf("target path %q must start with %q", path, "/")
	}
	if len(path) >= 2 && (path[1] == '/' || path[1] == '\\') {
		return "", "", false, fmt.Errorf("target path %q must not start with %q or %q: a doubled leading slash is a protocol-relative URL, and on a redirect route it points off-site", path, "//", "/\\")
	}
	if _, err := url.PathUnescape(path); err != nil {
		return "", "", false, fmt.Errorf("target path %q: %w", path, err)
	}
	if i := strings.IndexFunc(query, invalidQueryByte); i >= 0 {
		return "", "", false, fmt.Errorf("target query %q must not contain spaces or control characters (byte %#x at %d; escape it as %%20)", query, query[i], i)
	}
	return path, query, querySet, nil
}

// invalidQueryByte reports whether r cannot appear literally in a request
// target's query: the space and every ASCII control byte, including CR and LF.
func invalidQueryByte(r rune) bool {
	return r <= ' ' || r == 0x7f
}

// resolveReplacePathMW validates the fixed target path and splits off an
// explicit query.
func resolveReplacePathMW(m *replacePathMW) (resolved.Middleware, error) {
	path, query, querySet, err := splitReplacePath(m.path)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("replace_path: %w", err)
	}
	return resolved.Middleware{
		Type:            resolved.MWReplacePath,
		PathReplacement: path,
		PathQuery:       query,
		PathQuerySet:    querySet,
	}, nil
}

// resolveRewritePathMW compiles the rewrite pattern so a bad regexp is a
// startup error, and stores its canonical source. The replacement is carried
// through as written; an empty one deletes what the pattern matches.
func resolveRewritePathMW(m *rewritePathMW) (resolved.Middleware, error) {
	if m.pattern == "" {
		return resolved.Middleware{}, errors.New("rewrite_path: pattern must not be empty")
	}
	re, err := regexp.Compile(m.pattern)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("rewrite_path: %w", err)
	}
	return resolved.Middleware{
		Type:            resolved.MWRewritePath,
		PathPattern:     re.String(),
		PathReplacement: m.replacement,
	}, nil
}

// resolveAllowIPsMW canonicalises the allow-list CIDRs.
func resolveAllowIPsMW(m *allowIPsMW) (resolved.Middleware, error) {
	canon, err := resolveCIDRs(m.cidrs)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("allow_ips: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWAllowIPs, IPCIDRs: canon}, nil
}

// resolveDenyIPsMW canonicalises the deny-list CIDRs.
func resolveDenyIPsMW(m *denyIPsMW) (resolved.Middleware, error) {
	canon, err := resolveCIDRs(m.cidrs)
	if err != nil {
		return resolved.Middleware{}, fmt.Errorf("deny_ips: %w", err)
	}
	return resolved.Middleware{Type: resolved.MWDenyIPs, IPCIDRs: canon}, nil
}

// resolveBasicAuthMW requires a non-empty user map and verifies every
// value is a bcrypt hash.
func resolveBasicAuthMW(m *basicAuthMW) (resolved.Middleware, error) {
	if len(m.users) == 0 {
		return resolved.Middleware{}, errors.New("basic_auth: users map is empty")
	}
	users := make(map[string]string, len(m.users))
	for name, hash := range m.users {
		if !isBCryptHash(hash) {
			return resolved.Middleware{}, fmt.Errorf("basic_auth: user %q: value is not a bcrypt hash (must be $2a$/$2b$/$2y$ prefixed)", name)
		}
		users[name] = hash
	}
	return resolved.Middleware{
		Type:           resolved.MWBasicAuth,
		BasicAuthRealm: m.realm,
		BasicAuthUsers: users,
	}, nil
}

// resolveCORSMW splits explicit origins from the wildcard, rejects a
// credentialed wildcard, and parses the optional max-age.
func resolveCORSMW(m *corsMW) (resolved.Middleware, error) {
	if len(m.origins) == 0 {
		return resolved.Middleware{}, errors.New("cors: at least one Origin must be configured")
	}
	allowAll := false
	var explicit []string
	for _, o := range m.origins {
		if o == "*" {
			allowAll = true
			continue
		}
		explicit = append(explicit, o)
	}
	if allowAll && m.credentials {
		return resolved.Middleware{}, errors.New("cors: wildcard origin '*' is incompatible with Credentials() per the CORS spec")
	}
	maxAge := time.Duration(0)
	if m.maxAge != "" {
		d, err := parse.Duration(m.maxAge)
		if err != nil {
			return resolved.Middleware{}, fmt.Errorf("cors.max_age: %w", err)
		}
		maxAge = d
	}
	return resolved.Middleware{
		Type:               resolved.MWCORS,
		CORSAllowAllOrigin: allowAll,
		CORSOrigins:        explicit,
		CORSMethods:        append([]string(nil), m.methods...),
		CORSHeaders:        append([]string(nil), m.headers...),
		CORSExposeHeaders:  append([]string(nil), m.exposeHeaders...),
		CORSCredentials:    m.credentials,
		CORSMaxAge:         maxAge,
	}, nil
}

func resolveObservability(o Observability) (resolved.Observability, error) {
	out := resolved.Observability{}
	if o.AccessLog != nil {
		switch al := o.AccessLog.(type) {
		case *jsonLog:
			out.AccessLog = resolved.AccessLog{
				Enabled:    true,
				Format:     accessLogFormatJSON,
				Writer:     al.dest.Writer(),
				Name:       al.dest.Name(),
				SampleRate: al.sampleRate,
			}
		default:
			return resolved.Observability{}, fmt.Errorf("unknown access log type %T", o.AccessLog)
		}
	}
	if o.Metrics != nil {
		switch m := o.Metrics.(type) {
		case prometheusMetrics:
			if m.addr == "" {
				return resolved.Observability{}, errors.New("metrics: addr required")
			}
			path := m.path
			if path == "" {
				path = "/metrics"
			}
			out.Metrics = resolved.Metrics{
				Enabled: true,
				Kind:    "prometheus",
				Addr:    m.addr,
				Path:    path,
			}
		default:
			return resolved.Observability{}, fmt.Errorf("unknown metrics type %T", o.Metrics)
		}
	}
	if o.Tracing != nil {
		switch t := o.Tracing.(type) {
		case *otlpTracing:
			if t.endpoint == "" {
				return resolved.Observability{}, errors.New("tracing: endpoint required")
			}
			out.Tracing = resolved.Tracing{
				Enabled:     true,
				Kind:        "otlp",
				Endpoint:    t.endpoint,
				ServiceName: t.serviceName,
				Insecure:    t.insecure,
				SampleRate:  t.sampleRate,
			}
		default:
			return resolved.Observability{}, fmt.Errorf("unknown tracing type %T", o.Tracing)
		}
	}
	return out, nil
}

func resolveShutdown(s Shutdown) (resolved.Shutdown, error) {
	d, err := parse.DurationOr(s.GracePeriod, 30*time.Second)
	if err != nil {
		return resolved.Shutdown{}, fmt.Errorf("grace_period: %w", err)
	}
	return resolved.Shutdown{
		GracePeriod:    d,
		DrainListeners: s.DrainListeners,
	}, nil
}

func validateResolved(c *resolved.Config) error {
	if len(c.Listeners) == 0 {
		return errors.New("at least one listener required")
	}
	// Routes are optional in principle (a redirect-only deployment is valid),
	// but a redirect-only listener with no routes and no other listener is
	// almost certainly a misconfiguration.
	hasContentListener := false
	for _, l := range c.Listeners {
		if l.Redirect == "" {
			hasContentListener = true
			break
		}
	}
	if !hasContentListener && len(c.Routes) > 0 {
		return errors.New("routes declared but no listener serves content (all listeners are redirects)")
	}
	return nil
}

// resolveCIDRs validates and canonicalises a list of CIDR strings. Every
// entry must be parseable as a netip.Prefix; the resolved list is the
// canonical Masked form so that an entry like "10.0.0.1/24" is stored as
// "10.0.0.0/24".
func resolveCIDRs(cidrs []string) ([]string, error) {
	if len(cidrs) == 0 {
		return nil, errors.New("at least one CIDR required")
	}
	out := make([]string, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		out = append(out, p.Masked().String())
	}
	return out, nil
}
