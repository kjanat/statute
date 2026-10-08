package statute

import (
	"context"
	"io"
	"net/http"
)

type retryTrailerNamesKey struct{}

// A nonnil snapshot also records an empty declaration before asynchronous work.
type retryTrailerNames struct{ names []string }

type retryTrailerBody interface{ liveRetryTrailers() http.Header }

type retryTrailerReadCloser struct {
	io.ReadCloser
	trailers http.Header
}

func (b *retryTrailerReadCloser) liveRetryTrailers() http.Header { return b.trailers }

// Trailer ownership follows the body through built-in read wrappers.
func carryRetryTrailers(original, wrapped io.ReadCloser) io.ReadCloser {
	if source, ok := original.(retryTrailerBody); ok {
		return &retryTrailerReadCloser{ReadCloser: wrapped, trailers: source.liveRetryTrailers()}
	}
	return wrapped
}

func snapshotRetryTrailerNames(ctx context.Context, trailers http.Header) context.Context {
	names := make([]string, 0, len(trailers))
	for name := range trailers {
		names = append(names, name)
	}
	return context.WithValue(ctx, retryTrailerNamesKey{}, &retryTrailerNames{names: names})
}

func retryTrailerContext(ctx context.Context, trailers http.Header) context.Context {
	if _, ok := ctx.Value(retryTrailerNamesKey{}).(*retryTrailerNames); ok {
		return ctx
	}
	return snapshotRetryTrailerNames(ctx, trailers)
}

func announcedRetryTrailers(ctx context.Context) http.Header {
	snapshot := ctx.Value(retryTrailerNamesKey{}).(*retryTrailerNames)
	trailers := make(http.Header, len(snapshot.names))
	for _, name := range snapshot.names {
		trailers[name] = nil
	}
	return trailers
}
