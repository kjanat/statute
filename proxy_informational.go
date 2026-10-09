package statute

import (
	"errors"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
)

type proxyInformationalTransport struct{ base http.RoundTripper }

// A final HTTP/1 upgrade is not an informational transport callback.
func (t *proxyInformationalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	previous := httptrace.ContextClientTrace(r.Context())
	trace := &httptrace.ClientTrace{}
	ctx := httptrace.WithClientTrace(r.Context(), trace)
	// Replace only this composed hook, retaining every other caller hook.
	trace.Got1xxResponse = func(code int, header textproto.MIMEHeader) error {
		if code == http.StatusSwitchingProtocols {
			return errors.New("upstream sent an informational 101 response")
		}
		if previous != nil && previous.Got1xxResponse != nil {
			return previous.Got1xxResponse(code, header)
		}
		return nil
	}
	return t.base.RoundTrip(r.WithContext(ctx))
}
