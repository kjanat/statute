package statute

import (
	"context"
	"net/http"
)

type retryTrailerNamesKey struct{}

// A nonnil snapshot also records an empty declaration before asynchronous work.
type retryTrailerNames struct{ names []string }

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
