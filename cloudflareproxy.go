package statute

import (
	"context"
	"log"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"statute.kjanat.dev/internal/cloudflare"
	"statute.kjanat.dev/resolved"
)

const cloudflareRetry = 5 * time.Minute

// cloudflarePolicySnapshot is immutable after publication; all opted-in
// listeners share its ranges while keeping their own client-header policy.
type cloudflarePolicySnapshot struct {
	prefixes  []netip.Prefix
	fetchedAt time.Time
}

// cloudflareSource belongs to a server, never to the resolved configuration.
type cloudflareSource struct {
	current  atomic.Pointer[cloudflarePolicySnapshot]
	fallback cloudflare.Snapshot
	fetch    func(context.Context, *http.Client) (cloudflare.Snapshot, error)
	wait     func(context.Context, time.Duration) bool
}

// cloudflareSourceForListeners allocates one source only for provider opt-ins.
func cloudflareSourceForListeners(listeners []*resolved.Listener) *cloudflareSource {
	for _, listener := range listeners {
		if listener.CloudflareTrustedProxy {
			return newCloudflareSource(listenerCloudflareFallback(listener))
		}
	}
	return nil
}

// newCloudflareSource seeds the fail-closed fallback before handlers are built.
func newCloudflareSource(fallback cloudflare.Snapshot) *cloudflareSource {
	source := &cloudflareSource{fetch: cloudflare.Fetch, wait: waitCloudflareRefresh, fallback: fallback.Normalized()}
	source.publish(source.fallback)
	return source
}

// publish replaces the complete pair atomically, including removed prefixes.
func (s *cloudflareSource) publish(snapshot cloudflare.Snapshot) {
	s.current.Store(&cloudflarePolicySnapshot{
		prefixes:  mustParsePrefixes(snapshot.CIDRs()),
		fetchedAt: snapshot.FetchedAt,
	})
}

// middleware captures one immutable policy for the entire request, so ACLs,
// rate limits and logs agree even when a refresh replaces the live snapshot.
func (s *cloudflareSource) middleware(header string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot := s.current.Load()
		policy := &trustedProxyPolicy{prefixes: snapshot.prefixes, header: header}
		ctx := context.WithValue(r.Context(), tpCtxKey{}, policy)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// cloudflareRun owns exactly one periodic worker, its cancellation and transport.
type cloudflareRun struct {
	source *cloudflareSource
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	client *http.Client
}

// start refreshes synchronously before content serving and then owns polling.
// Each attempt starts from the fallback independently captured at construction.
func (s *cloudflareSource) start() *cloudflareRun {
	ctx, cancel := context.WithCancel(context.Background())
	run := &cloudflareRun{source: s, ctx: ctx, cancel: cancel, done: make(chan struct{}), client: cloudflare.NewClient()}
	s.publish(s.fallback)
	delay := run.refresh()
	go run.loop(delay)
	return run
}

// refresh bounds the whole pair and preserves the current snapshot on failure.
func (r *cloudflareRun) refresh() time.Duration {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	snapshot, err := r.source.fetch(ctx, r.client)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = snapshot.Validate()
	}
	if r.ctx.Err() != nil {
		return cloudflareRetry
	}
	if err != nil {
		log.Printf("statute: Cloudflare trusted proxies: refresh failed (%v); retaining snapshot fetched %s; retry in %s", err, r.source.current.Load().fetchedAt.Format(time.RFC3339), cloudflareRetry)
		return cloudflareRetry
	}
	r.source.publish(snapshot)
	return max(cloudflareRetry, min(24*time.Hour, snapshot.RefreshAfter))
}

// loop serializes bounded refreshes; cancellation interrupts both sleep and I/O.
func (r *cloudflareRun) loop(delay time.Duration) {
	defer close(r.done)
	defer r.client.CloseIdleConnections()
	for r.source.wait(r.ctx, delay) {
		delay = r.refresh()
	}
}

// stop waits for the request, timer and worker to retire before listener drain.
func (r *cloudflareRun) stop() {
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
}

// waitCloudflareRefresh owns and stops each timer, including cancellation paths.
func waitCloudflareRefresh(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
