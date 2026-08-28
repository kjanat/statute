# statute developer Makefile.
#
# Use this as the canonical interface for developer tasks; CI mirrors the
# same commands. If you find yourself reaching for a one-off command,
# consider adding a target here instead.

GO              ?= go
GOLANGCI_LINT   ?= golangci-lint
CUSTOM_GCL      ?= ./custom-gcl
COVER_PROFILE   ?= cover.out
FUZZ_TIME       ?= 30s
LIFECYCLE_BASE  ?= master
E2E_IMAGE       ?= statute-e2e:$(shell git rev-parse --short HEAD)
E2E_REPEAT      ?= 1
# A `go test` timeout panic bypasses teardown and the orphan proofs, so
# every tier's budget is overridable for slower runners and higher repeats.
E2E_TIMEOUT            ?= 30m
E2E_REGRESSION_TIMEOUT ?= 60m
E2E_SOAK_TIMEOUT       ?= 120m
COMMENT_COP_BASE       ?=

.PHONY: all help comment-cop test test-race fmt-check lint lint-lifecycle audit-lifecycle cover cover-html bench fuzz build-examples apidiff typecheck tidy clean e2e-image test-e2e test-e2e-regression test-e2e-soak e2e-clean

help:
	@awk 'BEGIN { FS = ":.*?## " } /^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: lint test cover build-examples ## Run lint, test, coverage, build examples

test: ## Run all unit tests
	$(GO) test ./...
	$(GO) -C traefikoracle test ./...

# No `-race` on 64-bit arm
test-race: ## Run tests with the race detector, skipped where TSAN cannot start
	@if probe="$$($(GO) test -race -run '^$$' -count=1 ./internal/parse 2>&1)"; then \
		echo "$(GO) test -race ./..."; \
		$(GO) test -race ./...; \
	elif printf '%s' "$$probe" | grep -q ThreadSanitizer; then \
		echo "test-race: skipped, this kernel cannot start ThreadSanitizer binaries."; \
		echo "           64-bit Arm with a 47-bit VMA does this; CI runs the"; \
		echo "           detector on x86, so the tier is not lost."; \
	else \
		printf '%s\n' "$$probe" >&2; \
		echo "test-race: the probe failed for a reason other than ThreadSanitizer" >&2; \
		exit 1; \
	fi

# Both checks use the active Go toolchain's go/printer. gofmt checks source
# form, while the pinned goimports tool additionally enforces import groups.
fmt-check: ## Fail if any tracked Go file is not formatted
	@if [ -z "$$(git ls-files -- '*.go')" ]; then \
		echo "fmt-check: no tracked Go files (not a git checkout?)" >&2; exit 1; fi; \
	if ! drift="$$(git ls-files -z -- '*.go' | xargs -0 -r gofmt -l --)"; then \
		echo "fmt-check: gofmt failed" >&2; exit 1; fi; \
	if [ -n "$$drift" ]; then echo "gofmt drift:"; echo "$$drift"; exit 1; fi; \
	if ! drift="$$(git ls-files -z -- '*.go' | xargs -0 -r $(GO) tool goimports -local statute.kjanat.dev -l --)"; then \
		echo "fmt-check: goimports failed" >&2; exit 1; fi; \
	if [ -n "$$drift" ]; then echo "goimports drift:"; echo "$$drift"; exit 1; fi

lint: fmt-check ## Check formatting, build the custom golangci-lint, run all linters
	$(GOLANGCI_LINT) custom
	$(CUSTOM_GCL) run ./...

lint-lifecycle: ## Reject lifecycle findings introduced since LIFECYCLE_BASE
	$(GOLANGCI_LINT) custom
	$(CUSTOM_GCL) run --enable-only=statutelifecycle --new-from-merge-base=$(LIFECYCLE_BASE) --whole-files ./...

audit-lifecycle: ## Report all lifecycle findings, including existing baseline debt
	$(GOLANGCI_LINT) custom
	$(CUSTOM_GCL) run --enable-only=statutelifecycle ./...

cover: ## Write coverage profile and print summary
	$(GO) test -coverprofile=$(COVER_PROFILE) -covermode=atomic ./...
	@echo "---"
	$(GO) tool cover -func=$(COVER_PROFILE) | tail -1

cover-html: cover ## Open coverage HTML in a browser
	$(GO) tool cover -html=$(COVER_PROFILE)

bench: ## Run all microbenchmarks
	$(GO) test -bench=. -benchmem -run=^$$ ./...

fuzz: ## Run every fuzz target for $(FUZZ_TIME)
	@set -e; \
	for target in $$(grep -rhoE '^func (Fuzz[A-Za-z0-9_]+)' --include='*_test.go' . | awk '{print $$2}' | sort -u); do \
		echo "== $$target =="; \
		$(GO) test -run=^$$ -fuzz="^$${target}$$" -fuzztime=$(FUZZ_TIME) ./...; \
	done

build-examples: ## Compile every example
	@set -e; \
	for ex in $$(ls examples); do \
		echo "build examples/$$ex"; \
		$(GO) build -o /dev/null ./examples/$$ex; \
	done

# The e2e lane is deliberately not part of `all`: it needs a Docker
# daemon and minutes of wall clock, and `all` has to keep working on a
# train. One image per commit; E2E_REPEAT=20 repeats the smoke matrix
# for the orphan/collision audit.
e2e-image: ## Build the black-box e2e image for this commit
	docker build -f e2e/Dockerfile -t $(E2E_IMAGE) .

test-e2e: e2e-image ## Run the e2e smoke matrix across all four topologies
	STATUTE_E2E_IMAGE=$(E2E_IMAGE) $(GO) test -tags e2e -count=$(E2E_REPEAT) -run '^TestSmoke' -timeout $(E2E_TIMEOUT) ./e2e

test-e2e-regression: e2e-image ## Run the deterministic e2e regression scenarios (PR gate)
	STATUTE_E2E_IMAGE=$(E2E_IMAGE) $(GO) test -tags e2e -count=1 -run '^Test(Smoke|Regression)' -timeout $(E2E_REGRESSION_TIMEOUT) ./e2e

test-e2e-soak: e2e-image ## Run the e2e stress/soak tier (scheduled lane)
	STATUTE_E2E_IMAGE=$(E2E_IMAGE) $(GO) test -tags e2e -count=1 -run '^TestSoak' -timeout $(E2E_SOAK_TIMEOUT) ./e2e

e2e-clean: ## Force-remove anything the e2e lane leaked (labeled containers, named projects)
	-docker ps -aq --filter label=statute.e2e=1 | xargs -r docker rm -f
	-docker network ls -q --filter name=statute-e2e- | xargs -r docker network rm
	-docker volume ls -q --filter name=statute-e2e- | xargs -r docker volume rm -f

# Deliberately not part of `all`: it reaches out to pkg.go.dev, and `all`
# has to keep working on a train. Pass -baseline to diff a saved surface
# offline; APIDIFF_ALLOW_BREAKING=1 downgrades a deliberate break to a warning.
apidiff: ## Diff the exported API against the surface published on pkg.go.dev
	$(GO) run ./scripts/apidiff

typecheck: ## Strict-typecheck scripts/ (installs dev deps on first run)
	@test -d node_modules || npm ci --no-audit --no-fund
	npm run typecheck

comment-cop: ## Flag paragraph-length Go comments in the working tree
	node scripts/comment-cop.mjs $(COMMENT_COP_BASE)

tidy: ## Run go mod tidy
	$(GO) mod tidy

clean: ## Remove build artefacts
	rm -f $(COVER_PROFILE) $(CUSTOM_GCL)
	$(GO) clean -testcache
