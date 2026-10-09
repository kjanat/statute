//go:build e2e

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

func TestNegativeProbeSetupErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-url", "/relative"},
		{"-url", "ftp://127.0.0.1/"},
		{"-url", "http:///missing-host"},
		{"-url", "http://user:pass@127.0.0.1/"},
		{"-url", "http://127.0.0.1:99999/"},
		{"-url", "http://127.0.0.1:0/"},
		{"-url", "http://127.0.0.1/", "-timeout", "0s"},
		{"-url", "http://127.0.0.1/", "-timeout", "-1s"},
		{"-url", "http://127.0.0.1/", "-proto", "unknown"},
		{"-url", "http://127.0.0.1/", "-proto", "h3"},
		{"-url", "http://127.0.0.1/", "-expect", "tls-rejection"},
		{"-url", "https://127.0.0.1/", "-expect", "anything"},
		{"-url", "https://127.0.0.1/", "-roots", filepath.Join(t.TempDir(), "missing")},
		{"-url", "http://127.0.0.1/", "unexpected"},
	} {
		if err := runProbeNegative(args); err == nil {
			t.Errorf("invalid setup passed: %q", args)
		}
	}
}

func TestNegativeProbeClosedTCP(t *testing.T) {
	ln := probeTCPListener(t)
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	for _, proto := range []string{"h1", "h2"} {
		if err := runProbeNegative([]string{"-url", "https://" + addr, "-proto", proto, "-timeout", "300ms"}); err != nil {
			t.Errorf("closed TCP %s: %v", proto, err)
		}
	}
	if err := runProbeNegative([]string{"-url", "https://" + addr, "-expect", "tls-rejection", "-timeout", "300ms"}); err == nil {
		t.Fatal("closed listener passed certificate rejection expectation")
	}
}

func TestNegativeProbeLiveHTTPAndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusFound} {
		ln := probeTCPListener(t)
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://127.0.0.1:1/")
			w.WriteHeader(status)
		})}
		serveProbeHTTP(t, server, ln)
		for _, scheme := range []string{"http", "https"} {
			if err := runProbeNegative([]string{"-url", scheme + "://" + ln.Addr().String(), "-timeout", "300ms"}); err == nil {
				t.Errorf("live HTTP %d over %s passed unavailability", status, scheme)
			}
		}
	}
}

func TestNegativeProbeConnectedFailures(t *testing.T) {
	for _, behavior := range []string{"silent", "eof", "malformed"} {
		t.Run(behavior, func(t *testing.T) {
			ln := probeTCPListener(t)
			release := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				switch behavior {
				case "silent":
					<-release
				case "malformed":
					_, _ = io.WriteString(conn, "this is not HTTP\r\n\r\n")
				}
			}()
			t.Cleanup(func() { close(release); ln.Close(); <-done })
			if err := runProbeNegative([]string{"-url", "http://" + ln.Addr().String(), "-timeout", "200ms"}); err == nil {
				t.Fatalf("connected %s passed unavailability", behavior)
			}
		})
	}
}

func TestNegativeProbeTLSCertificateAndRejection(t *testing.T) {
	cert, roots := probeCertificate(t)
	for _, requireClient := range []bool{false, true} {
		ln := probeTCPListener(t)
		cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
		if requireClient {
			cfg.ClientAuth = tls.RequireAnyClientCert
		}
		server := &http.Server{
			ReadHeaderTimeout: time.Second, ErrorLog: log.New(io.Discard, "", 0),
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		}
		serveProbeHTTP(t, server, tls.NewListener(ln, cfg))
		for _, proto := range []string{"h1", "h2"} {
			args := []string{"-url", "https://" + ln.Addr().String(), "-proto", proto, "-timeout", "500ms"}
			if err := runProbeNegative(args); err == nil {
				t.Fatal("untrusted server certificate passed unavailability")
			}
			if err := runProbeNegative(append(args, "-expect", "tls-rejection")); err == nil {
				t.Fatal("untrusted server certificate passed client certificate rejection")
			}
			args = append(args, "-roots", roots)
			if err := runProbeNegative(args); err == nil {
				t.Fatal("contacted TLS listener passed unavailability")
			}
			err := runProbeNegative(append(args, "-expect", "tls-rejection"))
			if (err == nil) != requireClient {
				t.Errorf("requireClient=%t proto=%s rejection error=%v", requireClient, proto, err)
			}
		}
	}
}

func TestNegativeProbeTLSRejectionALPN(t *testing.T) {
	cert, roots := probeCertificate(t)
	for _, proto := range []string{"h1", "h2"} {
		t.Run(proto, func(t *testing.T) {
			ln := probeTCPListener(t)
			offered := make(chan []string, 1)
			done := make(chan struct{})
			cfg := &tls.Config{
				Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
				ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{"h2", "http/1.1"},
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					offered <- slices.Clone(hello.SupportedProtos)
					return nil, nil
				},
			}
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_ = tls.Server(conn, cfg).Handshake()
			}()
			t.Cleanup(func() { ln.Close(); <-done })
			err := runProbeNegative([]string{
				"-url", "https://" + ln.Addr().String(), "-proto", proto,
				"-expect", "tls-rejection", "-roots", roots, "-timeout", "500ms",
			})
			if err != nil {
				t.Fatal(err)
			}
			want := "http/1.1"
			if proto == "h2" {
				want = "h2"
			}
			select {
			case got := <-offered:
				if !slices.Equal(got, []string{want}) {
					t.Fatalf("offered ALPN %q, want only %q", got, want)
				}
			default:
				t.Fatal("no TLS ClientHello observed")
			}
		})
	}
}

func TestNegativeProbeAcceptedTLSNeverWritesHTTP(t *testing.T) {
	cert, roots := probeCertificate(t)
	for _, proto := range []string{"h1", "h2"} {
		for _, behavior := range []string{"eof", "data", "stall"} {
			t.Run(proto+"/"+behavior, func(t *testing.T) {
				ln := probeTCPListener(t)
				result := make(chan error, 1)
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						result <- err
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(time.Second))
					secure := tls.Server(conn, &tls.Config{
						Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13,
						NextProtos: []string{"h2", "http/1.1"},
					})
					if err := secure.Handshake(); err != nil {
						result <- err
						return
					}
					switch behavior {
					case "data":
						_, err = secure.Write([]byte("x"))
					case "stall":
						var buf [1]byte
						if n, _ := secure.Read(buf[:]); n != 0 {
							err = errors.New("probe sent HTTP application data")
						}
					}
					result <- err
				}()
				if err := runProbeNegative([]string{
					"-url", "https://" + ln.Addr().String(), "-proto", proto,
					"-expect", "tls-rejection", "-roots", roots, "-timeout", "200ms",
				}); err == nil {
					t.Error("accepting TLS peer passed rejection expectation")
				}
				if err := <-result; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestNegativeProbeQUICClosedAndAnsweringPackets(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	if err := runProbeNegative([]string{"-url", "https://" + addr, "-proto", "h3", "-timeout", "300ms"}); err != nil {
		t.Fatalf("closed UDP: %v", err)
	}
	for _, reply := range []string{"", "malformed QUIC reply"} {
		t.Run("reply="+reply, func(t *testing.T) {
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				buf := make([]byte, 2048)
				for {
					_, peer, err := pc.ReadFrom(buf)
					if err != nil {
						return
					}
					_, _ = pc.WriteTo([]byte(reply), peer)
				}
			}()
			t.Cleanup(func() { pc.Close(); <-done })
			if err := runProbeNegative([]string{"-url", "https://" + pc.LocalAddr().String(), "-proto", "h3", "-timeout", "300ms"}); err == nil {
				t.Fatal("answering UDP service passed unavailability")
			}
		})
	}
}

func TestNegativeProbeQUICMTLS(t *testing.T) {
	cert, roots := probeCertificate(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http3.Server{
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert,
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(pc) }()
	t.Cleanup(func() { server.Close(); pc.Close(); <-done })
	args := make([]string, 0, 10)
	args = append(args, "-url", "https://"+pc.LocalAddr().String(), "-proto", "h3", "-roots", roots, "-timeout", "1s")
	if err := runProbeNegative(args); err == nil {
		t.Fatal("QUIC client certificate rejection passed unavailability")
	}
	if err := runProbeNegative(append(args, "-expect", "tls-rejection")); err != nil {
		t.Fatalf("QUIC certificate-required alert: %v", err)
	}
}

func TestNegativeProbeFailureClassification(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	for _, tc := range []struct {
		name      string
		proto     string
		contacted bool
		err       error
		want      bool
	}{
		{"refusal", "h1", false, &net.OpError{Op: "dial", Addr: addr, Err: syscall.ECONNREFUSED}, true},
		{"dial timeout", "h1", false, &net.OpError{Op: "dial", Addr: addr, Err: os.ErrDeadlineExceeded}, true},
		{"read timeout", "h1", false, &net.OpError{Op: "read", Addr: addr, Err: os.ErrDeadlineExceeded}, false},
		{"DNS", "h1", false, &net.OpError{Op: "dial", Addr: addr, Err: &net.DNSError{IsTimeout: true}}, false},
		{"setup", "h1", false, errors.New("bad configuration"), false},
		{"after contact", "h1", true, &net.OpError{Op: "dial", Addr: addr, Err: syscall.ECONNREFUSED}, false},
		{"QUIC handshake timeout", "h3", false, &quic.HandshakeTimeoutError{}, true},
		{"QUIC answering timeout", "h3", true, &quic.HandshakeTimeoutError{}, false},
		{"QUIC idle timeout", "h3", false, &quic.IdleTimeoutError{}, false},
		{"QUIC handshake silence", "h3", false, &negativeProbeQUICHandshakeError{err: &quic.IdleTimeoutError{}}, true},
		{"QUIC answering handshake silence", "h3", true, &negativeProbeQUICHandshakeError{err: &quic.IdleTimeoutError{}}, false},
		{"QUIC peer refusal", "h3", false, &quic.TransportError{Remote: true, ErrorCode: quic.ConnectionRefused}, false},
	} {
		if got := negativeProbeMatches("unavailable", tc.proto, tc.contacted, tc.err); got != tc.want {
			t.Errorf("%s: got %t want %t", tc.name, got, tc.want)
		}
	}
	for _, err := range []error{
		&quic.TransportError{Remote: false, ErrorCode: 0x100 + 116},
		&quic.TransportError{Remote: true, ErrorCode: 0x100 + 40},
		&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")},
		io.EOF,
		&net.OpError{Op: "write", Err: syscall.EPIPE},
		&net.OpError{Op: "read", Err: syscall.ECONNRESET},
		&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded},
	} {
		if negativeProbeMatches("tls-rejection", "h3", true, err) {
			t.Errorf("unrelated TLS error passed: %v", err)
		}
	}
}

func probeTCPListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func serveProbeHTTP(t *testing.T, server *http.Server, ln net.Listener) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ln) }()
	t.Cleanup(func() { server.Close(); ln.Close(); <-done })
}

func probeCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}
