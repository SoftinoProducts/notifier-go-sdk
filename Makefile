GO ?= go
GOLANGCI_LINT ?= golangci-lint
BENCHTIME ?= 300x
COVERAGE_FILE ?= coverage.out

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Compile every package
	$(GO) build ./...

.PHONY: test
test: ## Run the unit tests (offline, no network)
	$(GO) test ./...

.PHONY: race
race: ## Run the unit tests with the race detector
	$(GO) test -race ./...

.PHONY: cover
cover: ## Run the tests and report coverage per function
	$(GO) test -covermode=atomic -coverprofile=$(COVERAGE_FILE) ./...
	$(GO) tool cover -func=$(COVERAGE_FILE)

.PHONY: coverage
coverage: cover ## Open the HTML coverage report
	$(GO) tool cover -html=$(COVERAGE_FILE)

.PHONY: bench
bench: ## Run the local benchmarks with allocation reporting
	$(GO) test -run XXX -bench . -benchtime $(BENCHTIME) -benchmem ./...

.PHONY: live
live: ## Run the live tests against a real deployment (needs NOTIFIER_API_KEY)
	$(GO) test -run TestLive -v ./...

.PHONY: fmt
fmt: ## Format the source
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check: ## Fail if any file is not gofmt'd
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: fmt-check vet ## Run the formatter check, vet and golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: tidy
tidy: ## Tidy go.mod
	$(GO) mod tidy

.PHONY: check
check: lint race ## Everything CI runs on a pull request
