//go:build e2e

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	probeTLSRejection = "tls-rejection"
	clientSchemeHTTPS = "https"
)

// runProbeNegative proves absence of contact. Protocol, TLS and response failures
// after contacting a listener are failures, rather than evidence of rollback.
// The explicit tls-rejection expectation proves one particular remote TLS denial.
func runProbeNegative(args []string) error {
	fs := flag.NewFlagSet("probe-negative", flag.ContinueOnError)
	target := fs.String("url", "", "HTTP(S) URL to probe")
	proto := fs.String("proto", "h1", "h1, h2 or h3")
	timeout := fs.Duration("timeout", 3*time.Second, "positive per-attempt timeout")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	expect := fs.String("expect", "unavailable", "unavailable or tls-rejection (remote certificate-required alert)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	u, err := validateNegativeProbe(*target, *proto, *expect, *timeout)
	if err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("probe-negative: unexpected positional arguments")
	}
	tlsCfg, err := tlsConfigFor(*roots, "", "", "")
	if err != nil {
		return err
	}
	var contacted atomic.Bool
	transport, cleanup, err := negativeProbeTransport(u, *proto, tlsCfg, *timeout, &contacted)
	if err != nil {
		return err
	}
	defer cleanup()
	client := &http.Client{
		Timeout: *timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, probeErr := get(client, *target)
	if resp != nil {
		resp.Body.Close()
		return fmt.Errorf("probe-negative: %s answered over %s with status %d", *target, *proto, resp.StatusCode)
	}
	if !negativeProbeMatches(*expect, *proto, contacted.Load(), probeErr) {
		return fmt.Errorf("probe-negative: %s did not establish %s over %s (contacted=%t): %w",
			*target, *expect, *proto, contacted.Load(), probeErr)
	}
	event := "unreachable"
	if *expect == probeTLSRejection {
		event = "tls-rejected"
	}
	fmt.Printf(`{"event":%q,"url":%q,"proto":%q,"err":%q}`+"\n", event, *target, *proto, probeErr.Error())
	return nil
}

func validateNegativeProbe(target, proto, expect string, timeout time.Duration) (*url.URL, error) {
	u, err := negativeProbeURL(target)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, errors.New("probe-negative: -timeout must be positive")
	}
	switch proto {
	case "h1", "h2", "h3":
	default:
		return nil, fmt.Errorf("probe-negative: unknown proto %q", proto)
	}
	switch expect {
	case "unavailable", probeTLSRejection:
	default:
		return nil, fmt.Errorf("probe-negative: unknown expectation %q", expect)
	}
	if (proto == "h3" || expect == probeTLSRejection) && u.Scheme != clientSchemeHTTPS {
		return nil, errors.New("probe-negative: HTTP/3 and TLS rejection require an HTTPS URL")
	}
	return u, nil
}

func negativeProbeURL(target string) (*url.URL, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != clientSchemeHTTPS) || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("probe-negative: -url must be an absolute HTTP(S) URL without user information")
	}
	if !negativeProbePortValid(u.Port()) {
		return nil, errors.New("probe-negative: URL port must be between 1 and 65535")
	}
	return u, nil
}

func negativeProbePortValid(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func negativeProbeTransport(u *url.URL, proto string, tlsCfg *tls.Config, timeout time.Duration, contacted *atomic.Bool) (http.RoundTripper, func(), error) {
	if proto == "h3" {
		return negativeQUICTransport(context.Background(), u, tlsCfg, timeout, contacted)
	}
	dialer := &net.Dialer{Timeout: timeout}
	tr := &http.Transport{
		TLSClientConfig: tlsCfg, ForceAttemptHTTP2: proto == "h2",
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err == nil {
				// Record contact before TLS or HTTP gets an opportunity to fail.
				contacted.Store(true)
			}
			return conn, err
		},
	}
	return tr, tr.CloseIdleConnections, nil
}

func negativeProbeMatches(expect, proto string, contacted bool, err error) bool {
	if expect == probeTLSRejection {
		return contacted && certificateRequiredAlert(err)
	}
	if contacted {
		return false
	}
	if proto == "h3" {
		return negativeQUICUnavailable(err)
	}
	return negativeTCPUnavailable(err)
}

func negativeQUICUnavailable(err error) bool {
	var handshakeTimeout *quic.HandshakeTimeoutError
	var handshake *negativeProbeQUICHandshakeError
	var idleTimeout *quic.IdleTimeoutError
	return errors.As(err, &handshakeTimeout) || errors.Is(err, syscall.ECONNREFUSED) ||
		(errors.As(err, &handshake) && errors.As(handshake.err, &idleTimeout))
}

func negativeTCPUnavailable(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	// DNS/setup errors and non-dial timeouts never prove absence of a listener.
	return !errors.As(err, &dns) && errors.As(err, &op) && op.Op == "dial" && op.Addr != nil &&
		(errors.Is(err, syscall.ECONNREFUSED) || op.Timeout())
}

func certificateRequiredAlert(err error) bool {
	if remoteQUIC, ok := errors.AsType[*quic.TransportError](err); ok {
		// RFC 9001 encodes TLS alerts as CRYPTO_ERROR(0x100 + alert).
		return remoteQUIC.Remote && remoteQUIC.ErrorCode == 0x100+116
	}
	var remoteTCP *net.OpError
	return errors.As(err, &remoteTCP) && remoteTCP.Op == "remote error" &&
		remoteTCP.Err.Error() == "tls: certificate required"
}

// Packet receipt establishes contact even when QUIC discards a malformed packet
// and ultimately reports a handshake timeout. A plain timeout alone is too weak.
type negativeProbePacketConn struct {
	net.PacketConn
	contacted *atomic.Bool
}

// quic-go reports handshake silence as an IdleTimeoutError. Only errors from
// synchronous Dial (before handshake completion) can qualify that timeout.
type negativeProbeQUICHandshakeError struct{ err error }

func (e *negativeProbeQUICHandshakeError) Error() string { return e.err.Error() }
func (e *negativeProbeQUICHandshakeError) Unwrap() error { return e.err }

func (c *negativeProbePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if n > 0 || err == nil {
		c.contacted.Store(true)
	}
	return n, addr, err
}

func negativeQUICTransport(ctx context.Context, u *url.URL, cfg *tls.Config, timeout time.Duration, contacted *atomic.Bool) (*http3.Transport, func(), error) {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return nil, nil, err
	}
	packetConn, err := (&net.ListenConfig{}).ListenPacket(ctx, "udp", ":0")
	if err != nil {
		return nil, nil, err
	}
	qt := &quic.Transport{Conn: &negativeProbePacketConn{PacketConn: packetConn, contacted: contacted}}
	tr := &http3.Transport{
		TLSClientConfig: cfg,
		QUICConfig:      &quic.Config{HandshakeIdleTimeout: timeout / 2},
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, quicCfg *quic.Config) (*quic.Conn, error) {
			conn, err := qt.Dial(ctx, addr, tlsCfg, quicCfg)
			if err != nil {
				return nil, &negativeProbeQUICHandshakeError{err: err}
			}
			contacted.Store(true)
			return conn, nil
		},
	}
	return tr, func() { tr.Close(); qt.Close(); packetConn.Close() }, nil
}
