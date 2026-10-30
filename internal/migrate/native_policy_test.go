package migrate

import (
	"strings"
	"testing"

	statute "statute.kjanat.dev"
	"statute.kjanat.dev/resolved"
)

func TestNativeMiddlewarePoliciesGenerateCompleteRoutes(t *testing.T) {
	for _, policy := range []string{
		"replacePath: {path: /new%2Fpath}",
		"replacePathRegex: {regex: '^/old/(.*)', replacement: '/new/$1'}",
		"stripPrefix: {prefixes: [/api, /old], forceSlash: true}",
		"compress: {encodings: [gzip, br, zstd], defaultEncoding: gzip, minResponseBodyBytes: 8, includedContentTypes: [text/html]}",
		"compress: {minResponseBodyBytes: 0, excludedContentTypes: [application/json]}",
		"buffering: {disableRequestBuffer: true, disableResponseBuffer: true, retryExpression: 'Attempts() < 2'}",
		"ipAllowList: {sourceRange: [192.0.2.0/24], ipStrategy: {depth: 1, ipv6Subnet: 64}}",
		"ipAllowList: {sourceRange: [192.0.2.0/24], ipStrategy: {excludedIPs: [198.51.100.1], ipv6Subnet: 129}}",
		"rateLimit: {average: 2, sourceCriterion: {requestHeaderName: X-Client}}",
	} {
		t.Run(policy, func(t *testing.T) {
			r := audit(t, policyInputs(t, policy))
			requireMigrationGaps(t, r)
			if r.Report.GeneratedRoutes == 0 {
				t.Fatal("supported policy produced no route")
			}
			assertGeneratedParity(t, r)
		})
	}
}

func TestNativeMiddlewareInvalidValuesRemainGuarded(t *testing.T) {
	for _, policy := range []string{
		"replacePath: {path: /bad%ZZ}", "replacePathRegex: {regex: '[', replacement: /new}",
		"stripPrefix: {prefixes: [/api], forceSlash: invalid}",
		"compress: {encodings: false}", "compress: {encodings: []}", "compress: {minResponseBodyBytes: invalid}", "compress: {includedContentTypes: false}",
		"buffering: {memRequestBodyBytes: -1}", "buffering: {disableRequestBuffer: invalid}", "buffering: {retryExpression: '${PREDICATE}'}",
		"ipAllowList: {sourceRange: [192.0.2.0/24], ipStrategy: {depth: invalid}}",
		"ipAllowList: {sourceRange: [192.0.2.0/24], ipStrategy: {excludedIPs: false}}",
		"ipAllowList: {sourceRange: [192.0.2.0/24], ipStrategy: {ipv6Subnet: invalid}}",
		"rateLimit: {average: 2, burst: invalid}",
	} {
		r := audit(t, policyInputs(t, policy))
		if !r.Report.Blocking || !strings.Contains(string(r.Go), "migration incomplete") {
			t.Fatalf("invalid policy accepted: %s", policy)
		}
	}
}

func TestPublicHeaderAllowlistDoesNotExposeArbitraryValues(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		public      bool
	}{
		{"X-Forwarded-Proto", "https", true},
		{"X-Robots-Tag", "noindex, nofollow", true},
		{"X-Robots-Tag", "private-directive", false},
		{"Cache-Control", "public, max-age=3600, immutable", true},
		{"Cache-Control", "stale-if-error=60", true},
		{"Cache-Control", "unknown=value", false},
		{"Cache-Control", "max-age=bad", false},
		{"Cache-Control", "unknown", false},
		{"Authorization", "Bearer private-token", false},
	} {
		policy := "headers: {customResponseHeaders: {'" + tc.name + "': '" + tc.value + "'}}"
		r := audit(t, policyInputs(t, policy))
		if strings.Contains(string(r.Go), tc.value) != tc.public {
			t.Fatalf("header %s value exposure differs from its classification", tc.name)
		}
		if r.Report.Blocking == tc.public {
			t.Fatalf("header %s binding requirement lost", tc.name)
		}
	}
}

func TestImplicitDockerPortInventoryRejectsInvalidOrNonTCPPorts(t *testing.T) {
	for _, ports := range []string{"['invalid',8080]", "['8080/udp']", "[{target: 8080, protocol: udp}]"} {
		in := dockerServiceInputs(t, "")
		compose := "services:\n  api:\n    expose: " + ports + "\n    labels:\n      traefik.enable: 'true'\n      traefik.http.routers.api.rule: Host(`api.example`)\n"
		in.Compose = []string{fixture(t, "compose.yml", compose)}
		r := audit(t, in)
		if !r.Report.Blocking || r.Report.GeneratedRoutes != 0 || !strings.Contains(findings(r), "$service-selection") {
			t.Fatalf("unaudited HTTP backend selected for ports %s", ports)
		}
	}
}

func TestUnsupportedRuleStillReportsMissingService(t *testing.T) {
	body := strings.Replace(simpleDynamic, "Host(`api.example`) && Path(`/health`)", "HostRegexp(`.*`)", 1)
	body = strings.Replace(body, "service: api", "service: missing", 1)
	r := audit(t, basicInputs(t, body))
	if !strings.Contains(findings(r), "referenced service is absent") || r.Report.GeneratedRoutes != 0 {
		t.Fatal("unsupported rule hid its missing dependency")
	}
}

func TestBufferingReportsNativeBounds(t *testing.T) {
	for _, tc := range []struct {
		policy           string
		limits, inFlight bool
		maxRequest       int64
	}{
		{"buffering: {retryExpression: 'Attempts() < 2'}", true, true, 8 << 20},
		{"buffering: {maxRequestBodyBytes: 0, maxResponseBodyBytes: 2048}", true, true, 8 << 20},
		{"buffering: {maxRequestBodyBytes: 1024, maxResponseBodyBytes: 2048}", false, true, 1024},
		{"buffering: {maxRequestBodyBytes: 1024, disableResponseBuffer: true}", false, true, 1024},
		{"buffering: {disableRequestBuffer: true, disableResponseBuffer: true}", false, false, 8 << 20},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			r := audit(t, policyInputs(t, tc.policy))
			requireMigrationGaps(t, r)
			notes := map[string]bool{}
			for _, o := range r.Report.Objects {
				for _, f := range o.Fields {
					notes[f.Path] = true
				}
			}
			if notes["$body-limits"] != tc.limits || notes["$in-flight"] != tc.inFlight {
				t.Fatalf("notes=%v\n%s", notes, findings(r))
			}
			c, err := statute.Resolve(r.Config)
			if err != nil {
				t.Fatal(err)
			}
			p := bodyBufferPolicy(c.Routes[0].Middleware)
			if p == nil || p.MaxRequestBytes != tc.maxRequest || p.MaxInFlight != 128 {
				t.Fatalf("resolved policy=%+v", p)
			}
		})
	}
}

func bodyBufferPolicy(mws []resolved.Middleware) *resolved.BodyBufferPolicy {
	for _, m := range mws {
		if m.BodyBuffers != nil {
			return m.BodyBuffers
		}
		if p := bodyBufferPolicy(m.Group); p != nil {
			return p
		}
	}
	return nil
}
