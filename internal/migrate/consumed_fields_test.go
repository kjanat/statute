package migrate

import (
	"strings"
	"testing"
)

func TestUnconsumedNativeMiddlewareFieldsBlockMigration(t *testing.T) {
	for _, policy := range []string{
		"buffering: {maxRequestBodyByte: 4}",
		"buffering: {maxRequestBodyBytes: 4, retryExpressions: 'Attempts() < 2'}",
		"buffering: {retryExpression: {unknown: value}}",
		"buffering: {retryExpression: false}",
		"compress: {minResponseBodyByte: 4}",
		"compress: {encodings: [br], unknown: true}",
		"compress: {defaultEncoding: {unknown: value}}",
		"compress: {defaultEncoding: false}",
		"ipAllowList: {sourceRange: [192.0.2.0/24], ipStrategy: {deppth: 1}}",
		"rateLimit: {average: 2, sourceCriterion: {ipStrategy: {deppth: 1}}}",
	} {
		t.Run(policy, func(t *testing.T) {
			r := audit(t, policyInputs(t, policy))
			if !r.Report.Blocking || !strings.Contains(string(r.Go), "migration incomplete") {
				t.Fatal("unconsumed middleware policy approved for serving")
			}
		})
	}
}

func TestDockerConsumedNativeMiddlewareFieldsRemainSupported(t *testing.T) {
	for _, fields := range []string{
		"buffering.disableRequestBuffer: 'true'\n      traefik.http.middlewares.candidate.buffering.disableResponseBuffer: 'true'",
		"compress.encodings: 'br,zstd'\n      traefik.http.middlewares.candidate.compress.defaultEncoding: br\n      traefik.http.middlewares.candidate.compress.minResponseBodyBytes: '1'\n      traefik.http.middlewares.candidate.compress.includedContentTypes: text/plain",
		"ipallowlist.sourcerange: '192.0.2.0/24'\n      traefik.http.middlewares.candidate.ipallowlist.ipstrategy.depth: '1'",
		"ratelimit.average: '2'\n      traefik.http.middlewares.candidate.ratelimit.sourcecriterion.ipstrategy.depth: '1'",
	} {
		labels := "      traefik.http.routers.api.middlewares: candidate\n      traefik.http.middlewares.candidate." + fields + "\n"
		r := audit(t, dockerServiceInputs(t, labels))
		if r.Report.Blocking {
			t.Fatalf("consumed Docker fields blocked: %s", findings(r))
		}
		assertGeneratedParity(t, r)
	}
}

func TestDockerNativeMiddlewareUnknownFieldsBlockMigration(t *testing.T) {
	for _, field := range []string{"buffering.maxRequestBodyByte", "compress.minResponseBodyByte", "ipallowlist.ipstrategy.deppth", "ratelimit.sourcecriterion.ipstrategy.deppth"} {
		labels := "      traefik.http.routers.api.middlewares: candidate\n      traefik.http.middlewares.candidate." + field + ": '4'\n"
		if strings.HasPrefix(field, "ipallowlist") {
			labels += "      traefik.http.middlewares.candidate.ipallowlist.sourcerange: '192.0.2.0/24'\n"
		}
		if strings.HasPrefix(field, "ratelimit") {
			labels += "      traefik.http.middlewares.candidate.ratelimit.average: '2'\n"
		}
		r := audit(t, dockerServiceInputs(t, labels))
		if !r.Report.Blocking || !strings.Contains(string(r.Go), "migration incomplete") {
			t.Fatalf("unconsumed Docker field %s approved for serving", field)
		}
	}
}
