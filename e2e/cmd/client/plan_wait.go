//go:build e2e

package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"statute.kjanat.dev/e2e/report"
)

func executeWaitStep(step *report.Step) report.Result {
	result := report.Result{Step: step.Name, URL: step.URL, TargetServer: step.TargetServer, Proto: step.Proto}
	start := time.Now()
	body, status, err := waitStep(step)
	result.Latency = time.Since(start)
	if err != nil {
		result.Err = err.Error()
		return result
	}
	result.OK = true
	result.Status = status
	result.Body = string(body)
	return result
}

func validateWaitStep(step *report.Step) error {
	if step.Count < 0 || step.Count > 1 || step.Concurrency < 0 || step.Concurrency > 1 ||
		(step.Method != "" && step.Method != http.MethodGet) || waitStepHasRequestOptions(step) {
		return errors.New("wait step requires one GET and predicates in wait.spec")
	}
	return nil
}

func waitStepHasRequestOptions(step *report.Step) bool {
	return len(step.Headers) != 0 || len(step.Trailers) != 0 || step.Body != "" || step.Expect != (report.Expect{})
}

func waitStep(step *report.Step) ([]byte, int, error) {
	if err := validateWaitStep(step); err != nil {
		return nil, 0, err
	}
	timeout := 30 * time.Second
	if step.Wait.Timeout != "" {
		var err error
		timeout, err = time.ParseDuration(step.Wait.Timeout)
		if err != nil || timeout <= 0 {
			return nil, 0, errors.New("wait step requires a positive timeout")
		}
	}
	predicate, err := compileWaitSpec(step.Wait.Spec)
	if err != nil {
		return nil, 0, err
	}
	client, closeClient, err := clientFor(step)
	if err != nil {
		return nil, 0, err
	}
	defer closeClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	body, err := pollWait(ctx, client, step.URL, predicate, 500*time.Millisecond)
	return body, predicate.spec.Status, err
}
