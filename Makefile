.PHONY: build lint test test-unit test-integration coverage clean help

## ——— Build ————————————————————————————————————————————

build: ## Compile all packages
	go build ./...

lint: ## Run golangci-lint (install: brew install golangci-lint)
	golangci-lint run ./...

build-lint: build lint ## Build + lint (mirrors CI)

## ——— Tests ————————————————————————————————————————————

test-unit: ## Run unit tests (human-readable output via gotestsum)
	gotestsum --format testdox -- -count=1 -race ./tests/unit/...

test-integration: ## Run integration tests (requires Docker)
	gotestsum --format testdox -- -count=1 -timeout 1000s ./tests/integration/...

test: test-unit test-integration ## Run all tests

## ——— Coverage —————————————————————————————————————————

coverage: ## Generate coverage report (unit + integration, requires Docker)
	go test -count=1 -timeout 1000s \
		-coverprofile=coverage.out -covermode=atomic \
		-coverpkg=./... ./tests/...
	go tool cover -func=coverage.out

coverage-html: coverage ## Open coverage report in browser
	go tool cover -html=coverage.out

## ——— Deps —————————————————————————————————————————————

deps: ## Download Go modules
	go mod download

tools: ## Install dev tools (gotestsum, golangci-lint)
	go install gotest.tools/gotestsum@latest
	@echo "--------------------------------------------"
	@echo "golangci-lint is best installed via brew:"
	@echo "  brew install golangci-lint"
	@echo "--------------------------------------------"

## ——— Misc —————————————————————————————————————————————

clean: ## Remove generated files
	rm -f coverage.out

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help

