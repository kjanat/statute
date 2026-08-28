//go:build e2e

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"

	"statute.kjanat.dev/e2e/report"
)

// runPlan executes a request plan from the shared mount and writes the
// structured report back. Every planned edge with zero successful
// traversals is a failure, and any failure makes the process exit
// non-zero, so a silently missing edge can never look green.
func runPlan(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	planPath := fs.String("plan", "", "JSON plan file")
	outPath := fs.String("out", "", "report file to write")
	fs.Parse(args)
	if *planPath == "" || *outPath == "" {
		return errors.New("run: -plan and -out required")
	}
	raw, err := os.ReadFile(*planPath)
	if err != nil {
		return err
	}
	var plan report.Plan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return fmt.Errorf("run: parse plan: %w", err)
	}
	rep := executePlan(&plan)
	blob, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outPath, blob, 0o644); err != nil {
		return err
	}
	fmt.Printf(`{"event":"report","client":%q,"plan":%q,"results":%d,"failures":%d}`+"\n",
		rep.ClientID, rep.Plan, len(rep.Results), len(rep.Failures))
	if len(rep.Failures) > 0 {
		return fmt.Errorf("run: %d step(s) with no successful traversal: %s",
			len(rep.Failures), strings.Join(rep.Failures, ", "))
	}
	return nil
}

func executePlan(plan *report.Plan) *report.Report {
	rep := &report.Report{
		ClientID: plan.ClientID,
		Plan:     plan.Name,
		Topology: plan.Topology,
		Started:  time.Now(),
	}
	for i := range plan.Steps {
		step := &plan.Steps[i]
		results := executeStep(plan, step)
		succeeded := false
		for _, r := range results {
			if r.OK {
				succeeded = true
				break
			}
		}
		if !succeeded {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s->%s", step.Name, step.TargetServer))
		}
		rep.Results = append(rep.Results, results...)
	}
	rep.Finished = time.Now()
	return rep
}

func executeStep(plan *report.Plan, step *report.Step) []report.Result {
	count := max(step.Count, 1)
	workers := min(max(step.Concurrency, 1), count)
	client, closeClient, err := clientFor(step)
	if err != nil {
		return []report.Result{{
			Step: step.Name, URL: step.URL, TargetServer: step.TargetServer,
			Proto: step.Proto, Err: err.Error(),
		}}
	}
	defer closeClient()

	results := make([]report.Result, count)
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i := range count {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = doRequest(client, plan, step, i)
		})
	}
	wg.Wait()
	return results
}

// clientFor builds one HTTP client per step so connection reuse happens
// within a step (the realistic browser/SDK shape) and never across
// protocol boundaries.
func clientFor(step *report.Step) (*http.Client, func(), error) {
	tlsCfg, err := tlsConfigFor(step.RootsFile, step.ServerName, step.ClientCertFile, step.ClientKeyFile)
	if err != nil {
		return nil, nil, err
	}
	switch step.Proto {
	case "h3":
		tr := &http3.Transport{TLSClientConfig: tlsCfg}
		return &http.Client{Timeout: 30 * time.Second, Transport: tr}, func() { tr.Close() }, nil
	case "h2":
		tr := &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true}
		return &http.Client{Timeout: 30 * time.Second, Transport: tr}, tr.CloseIdleConnections, nil
	case "h1", "":
		tr := &http.Transport{
			TLSClientConfig: tlsCfg,
			// A non-nil empty map disables the h2 upgrade so "h1" really
			// means HTTP/1.1 even against an h2-capable listener.
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		}
		return &http.Client{Timeout: 30 * time.Second, Transport: tr}, tr.CloseIdleConnections, nil
	default:
		return nil, nil, fmt.Errorf("unknown proto %q", step.Proto)
	}
}

func doRequest(client *http.Client, plan *report.Plan, step *report.Step, i int) report.Result {
	res := report.Result{
		Step: step.Name, URL: step.URL, TargetServer: step.TargetServer, Proto: step.Proto,
		RequestID: fmt.Sprintf("%s-%s-%d-%d", plan.ClientID, step.Name, plan.Seed, i),
	}
	method := step.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(context.Background(), method, step.URL, strings.NewReader(step.Body))
	if err != nil {
		res.Err = err.Error()
		return res
	}
	req.Header.Set("X-Request-Id", res.RequestID)
	for k, v := range step.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := client.Do(req)
	res.Latency = time.Since(start)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.Status = resp.StatusCode
	res.NegotiatedProto = resp.Proto
	res.OriginID = parseOriginID(body)
	res.OK = stepExpectMet(step, resp.StatusCode, body)
	if !res.OK {
		res.Err = fmt.Sprintf("expectation not met: status %d", resp.StatusCode)
	}
	return res
}

func stepExpectMet(step *report.Step, status int, body []byte) bool {
	if step.Expect.Status != 0 && status != step.Expect.Status {
		return false
	}
	if step.Expect.Status == 0 && status >= 400 {
		return false
	}
	if step.Expect.BodyContains != "" && !strings.Contains(string(body), step.Expect.BodyContains) {
		return false
	}
	return true
}

// parseOriginID pulls the serving origin's identity out of an echo body;
// non-echo responses simply have none.
func parseOriginID(body []byte) string {
	var echo struct {
		Origin string `json:"origin"`
	}
	if json.Unmarshal(body, &echo) != nil {
		return ""
	}
	return echo.Origin
}
