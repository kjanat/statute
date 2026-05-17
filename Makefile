# statute developer Makefile.
#
# Use this as the canonical interface for developer tasks; CI mirrors the
# same commands. If you find yourself reaching for a one-off command,
# consider adding a target here instead.

GO              ?= go
GOLANGCI_LINT   ?= golangci-lint
COVER_PROFILE   ?= cover.out
FUZZ_TIME       ?= 30s

.PHONY: all help test test-race lint cover bench fuzz build-examples tidy clean

help:
	@awk 'BEGIN { FS = ":.*?## " } /^[a-zA-Z_-]+:.*?## / { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

all: lint test cover build-examples ## Run lint, test, coverage, build examples

test: ## Run all unit tests
	$(GO) test ./...

test-race: ## Run tests with the race detector (x86 only; Pi cannot run -race)
	$(GO) test -race ./...

lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run ./...

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

tidy: ## Run go mod tidy
	$(GO) mod tidy

clean: ## Remove build artefacts
	rm -f $(COVER_PROFILE)
	$(GO) clean -testcache
