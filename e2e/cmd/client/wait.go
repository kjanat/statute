//go:build e2e

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const waitBodyLimit = 1 << 20

// runWait keeps repeated network observations within one client actor.
func runWait(args []string) error {
	fs := flag.NewFlagSet("wait", flag.ContinueOnError)
	target := fs.String("url", "", "URL to poll")
	timeout := fs.Duration("timeout", 30*time.Second, "polling deadline")
	roots := fs.String("roots", "", "PEM roots for HTTPS targets")
	cert := fs.String("cert", "", "PEM client certificate for mTLS targets")
	key := fs.String("key", "", "PEM private key for the client certificate")
	spec := fs.String("spec", "", "JSON observation predicates; success prints the final body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" || *timeout <= 0 {
		return errors.New("wait: -url and positive -timeout required")
	}
	predicate, err := parseWaitSpec(*spec)
	if err != nil {
		return err
	}
	tlsCfg, err := tlsConfigFor(*roots, "", *cert, *key)
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	interval := 500 * time.Millisecond
	if *spec == "" {
		interval = 250 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	body, err := pollWait(ctx, client, *target, predicate, interval)
	if err != nil {
		return err
	}
	if *spec != "" {
		_, err = os.Stdout.Write(body)
	} else {
		_, err = fmt.Printf(`{"event":"ready","url":%q}`+"\n", *target)
	}
	return err
}

func pollWait(ctx context.Context, client *http.Client, target string, predicate *waitPredicate, interval time.Duration) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	consecutive := 0
	last := "no observation"
	for ctx.Err() == nil {
		status, body, requestErr := waitObservation(ctx, client, req)
		consecutive, last, err = waitProgress(predicate, status, body, requestErr, consecutive)
		if err != nil {
			return nil, err
		}
		if consecutive >= predicate.spec.Consecutive {
			return body, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("wait: %s: %w; last observation: %s", target, ctx.Err(), last)
}

func waitProgress(predicate *waitPredicate, status int, body []byte, requestErr error, consecutive int) (int, string, error) {
	if err := predicate.rejectBody(body); err != nil {
		return 0, "", err
	}
	if len(body) > waitBodyLimit {
		return 0, "", errors.New("wait: response exceeds 1 MiB limit")
	}
	var last string
	if requestErr == nil {
		matched, err := predicate.matches(status, body)
		if err != nil {
			return 0, "", err
		}
		consecutive = nextWaitCount(matched, consecutive)
		last = fmt.Sprintf("status %d, body %.512q", status, body)
	} else {
		consecutive = 0
		last = requestErr.Error()
	}
	return consecutive, last, nil
}

func nextWaitCount(matched bool, previous int) int {
	if matched {
		return previous + 1
	}
	return 0
}

func waitObservation(ctx context.Context, client *http.Client, req *http.Request) (int, []byte, error) {
	attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := client.Do(req.Clone(attempt))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, waitBodyLimit+1))
	return resp.StatusCode, body, err
}
