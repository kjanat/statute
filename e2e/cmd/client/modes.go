//go:build e2e

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"
)

// tlsConfigFor builds the client TLS policy for one target: an explicit
// PEM bundle when the scenario provides one (the harness PKI or a
// Pebble-issued root), system roots otherwise.
func tlsConfigFor(rootsFile, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	if rootsFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(rootsFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in %s", rootsFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// get issues one GET carrying a background context; the per-attempt
// deadline lives on the client.
func get(client *http.Client, target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// runGet performs one GET and writes the raw response body to stdout —
// the harness's generic in-network read for origin admin endpoints,
// journals, and metrics. A non-2xx status is an error exit.
func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	target := fs.String("url", "", "URL to fetch")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	fs.Parse(args)
	if *target == "" {
		return errors.New("get: -url required")
	}
	tlsCfg, err := tlsConfigFor(*roots, "")
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	resp, err := get(client, *target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("get: status %d", resp.StatusCode)
	}
	return nil
}

// runWait polls a URL until it answers 200 or the deadline passes. It is
// the lane's readiness gate: reachability of the Statute health
// endpoint's ready path over the real network, never container state.
func runWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	target := fs.String("url", "", "URL to poll until it returns 200")
	timeout := fs.Duration("timeout", 30*time.Second, "polling deadline")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	fs.Parse(args)
	if *target == "" {
		return errors.New("wait: -url required")
	}
	tlsCfg, err := tlsConfigFor(*roots, "")
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}
	deadline := time.Now().Add(*timeout)
	var last error
	for time.Now().Before(deadline) {
		resp, err := get(client, *target)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				fmt.Printf(`{"event":"ready","url":%q}`+"\n", *target)
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("wait: %s not ready within %s: %w", *target, *timeout, last)
}

// runProbeNegative succeeds only when the target is NOT reachable: a
// refused or timed-out connection is the pass condition. It proves a
// released listener (TCP or QUIC/UDP) and a failed startup that never
// exposed a route.
func runProbeNegative(args []string) error {
	fs := flag.NewFlagSet("probe-negative", flag.ExitOnError)
	target := fs.String("url", "", "URL that must not answer")
	proto := fs.String("proto", "h1", "h1 or h3")
	timeout := fs.Duration("timeout", 3*time.Second, "per-attempt timeout")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	fs.Parse(args)
	if *target == "" {
		return errors.New("probe-negative: -url required")
	}
	tlsCfg, err := tlsConfigFor(*roots, "")
	if err != nil {
		return err
	}
	// A redirect would prove some other endpoint answered, not this one.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var probeErr error
	switch *proto {
	case "h3":
		tr := &http3.Transport{TLSClientConfig: tlsCfg}
		defer tr.Close()
		client := &http.Client{Timeout: *timeout, Transport: tr, CheckRedirect: noRedirect}
		probeErr = drainGet(client, *target)
	case "h1":
		client := &http.Client{
			Timeout:       *timeout,
			Transport:     &http.Transport{TLSClientConfig: tlsCfg},
			CheckRedirect: noRedirect,
		}
		probeErr = drainGet(client, *target)
	default:
		return fmt.Errorf("probe-negative: unknown proto %q", *proto)
	}
	if probeErr == nil {
		return fmt.Errorf("probe-negative: %s unexpectedly answered over %s", *target, *proto)
	}
	fmt.Printf(`{"event":"unreachable","url":%q,"proto":%q,"err":%q}`+"\n", *target, *proto, probeErr.Error())
	return nil
}

func drainGet(client *http.Client, target string) error {
	resp, err := get(client, target)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

// runFetchRoots downloads a CA root PEM from inside the network —
// Pebble's per-run issuance root — so a later plan can verify the
// certificates Statute serves. The management endpoint's own TLS is
// self-signed, hence the insecure toggle; the fetched root is then
// verified by use.
func runFetchRoots(args []string) error {
	fs := flag.NewFlagSet("fetch-roots", flag.ExitOnError)
	target := fs.String("url", "", "root PEM URL")
	out := fs.String("out", "", "file to write the PEM to")
	insecure := fs.Bool("insecure", false, "skip TLS verification of the management endpoint")
	fs.Parse(args)
	if *target == "" || *out == "" {
		return errors.New("fetch-roots: -url and -out required")
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			//nolint:gosec // fetching a test CA's root over its self-signed management endpoint
			TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure, MinVersion: tls.VersionTLS12},
		},
	}
	resp, err := get(client, *target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch-roots: status %d", resp.StatusCode)
	}
	pem, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, pem, 0o644); err != nil {
		return err
	}
	fmt.Printf(`{"event":"roots","url":%q,"out":%q,"bytes":%d}`+"\n", *target, *out, len(pem))
	return nil
}

// runStream reads a chunked response and reports first-byte versus total
// latency; a proxy that buffers the whole body fails the early-first-
// chunk expectation.
func runStream(args []string) error {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	target := fs.String("url", "", "streaming URL")
	chunks := fs.Int("chunks", 5, "expected line count")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	fs.Parse(args)
	if *target == "" {
		return errors.New("stream: -url required")
	}
	tlsCfg, err := tlsConfigFor(*roots, "")
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	start := time.Now()
	resp, err := get(client, *target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var (
		firstByte time.Duration
		lines     int
	)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if lines == 0 {
			firstByte = time.Since(start)
		}
		lines++
	}
	total := time.Since(start)
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Printf(`{"event":"stream","lines":%d,"first_byte_ms":%d,"total_ms":%d}`+"\n",
		lines, firstByte.Milliseconds(), total.Milliseconds())
	if lines != *chunks {
		return fmt.Errorf("stream: got %d lines, want %d", lines, *chunks)
	}
	// The origin spaces its chunks out, so only a buffering proxy holds
	// the first byte past the halfway mark.
	if total > 0 && firstByte*2 > total {
		return fmt.Errorf("stream: first byte at %s of %s total suggests buffering", firstByte, total)
	}
	return nil
}

// runUpgrade performs a raw HTTP/1.1 connection upgrade through the
// proxy and round-trips lines over the switched protocol.
func runUpgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	target := fs.String("url", "", "upgrade URL")
	lines := fs.Int("lines", 3, "lines to round-trip")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	fs.Parse(args)
	if *target == "" {
		return errors.New("upgrade: -url required")
	}
	u, err := url.Parse(*target)
	if err != nil {
		return err
	}
	conn, err := dialUpgrade(u, *roots)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	br, err := upgradeHandshake(conn, u)
	if err != nil {
		return err
	}
	if err := echoRoundTrips(conn, br, *lines); err != nil {
		return err
	}
	fmt.Printf(`{"event":"upgrade","lines":%d}`+"\n", *lines)
	return nil
}

// upgradeHandshake sends the raw upgrade request and consumes the 101
// response head, leaving the reader positioned on the switched protocol.
func upgradeHandshake(conn net.Conn, u *url.URL) (*bufio.Reader, error) {
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n", u.RequestURI(), u.Host)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.Contains(status, "101") {
		return nil, fmt.Errorf("upgrade: got %q, want 101", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if line == "\r\n" {
			return br, nil
		}
	}
}

func echoRoundTrips(conn net.Conn, br *bufio.Reader, lines int) error {
	for i := range lines {
		msg := fmt.Sprintf("ping %d\n", i)
		if _, err := io.WriteString(conn, msg); err != nil {
			return err
		}
		echo, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasSuffix(echo, msg) {
			return fmt.Errorf("upgrade: echo %q does not end with %q", echo, msg)
		}
	}
	return nil
}

func dialUpgrade(u *url.URL, roots string) (net.Conn, error) {
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	ctx := context.Background()
	if u.Scheme != "https" {
		return d.DialContext(ctx, "tcp", host)
	}
	tlsCfg, err := tlsConfigFor(roots, u.Hostname())
	if err != nil {
		return nil, err
	}
	td := &tls.Dialer{NetDialer: d, Config: tlsCfg}
	return td.DialContext(ctx, "tcp", host)
}
