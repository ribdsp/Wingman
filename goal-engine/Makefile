# Wingman goal engine.
#
# Run `make` with no target for the list. Every target here is something you would
# otherwise have to remember the flags for; nothing here hides a decision.

BINARY      := goal-engine
CMD         := ./cmd/goal-engine
BIN_DIR     := bin
IMAGE       := wingman/goal-engine

# Version is taken from git and stamped into the binary, so a running process can
# say which commit it is. A dirty tree says so too, rather than claiming the tag.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)

GO          ?= go
COVER_FILE  := coverage.out

.DEFAULT_GOAL := help
.PHONY: help tidy fmt vet lint test test-race cover cover-html build run docker-build docker-run migrate-down clean

help: ## Show this help
	@echo "Wingman goal engine — version $(VERSION)"
	@echo
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

tidy: ## Sync go.mod and go.sum
	$(GO) mod tidy

fmt: ## Format every Go file
	$(GO) fmt ./...

vet: ## Run go vet
	$(GO) vet ./...

lint: ## Run golangci-lint (install: https://golangci-lint.run)
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint is not installed; skipping"; exit 0; }
	golangci-lint run ./...

test: ## Run the test suite
	$(GO) test ./...

# The race detector needs cgo and a C toolchain. It is a separate target rather
# than the default because a contributor without gcc should still be able to run
# the tests — CI runs this one on every push, so nothing merges unraced.
test-race: ## Run the test suite under the race detector (needs cgo + gcc)
	CGO_ENABLED=1 $(GO) test -race ./...

cover: ## Run tests with coverage and print the total
	$(GO) test -coverprofile=$(COVER_FILE) -covermode=atomic ./...
	@$(GO) tool cover -func=$(COVER_FILE) | tail -1

cover-html: cover ## Open the coverage report in a browser
	$(GO) tool cover -html=$(COVER_FILE)

build: ## Compile the binary into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD)

run: ## Run the service from source, reading .env
	$(GO) run -ldflags="$(LDFLAGS)" $(CMD)

docker-build: ## Build the container image
	docker build -t $(IMAGE):$(VERSION) -t $(IMAGE):dev --build-arg VERSION=$(VERSION) .

# Host networking is not assumed: the container talks to a database and to Wingman
# core over whatever DATABASE_URL and WINGMAN_CORE_BASE_URL name, which inside a
# container is not localhost. Use deploy/docker-compose.yml for a full stack.
docker-run: ## Run the image with the local .env
	docker run --rm -p 8080:8080 --env-file .env $(IMAGE):dev

# Migrations apply on boot when AUTO_MIGRATE is on, so there is no up target here.
# Rolling one back is a deliberate act and needs the golang-migrate CLI:
#   go install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@latest
migrate-down: ## Roll back the last migration (needs the migrate CLI and DATABASE_URL)
	@command -v migrate >/dev/null 2>&1 || { echo "the migrate CLI is not installed; see the comment above this target"; exit 1; }
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is not set"; exit 1; }
	migrate -path migrations -database "$(DATABASE_URL)" down 1

clean: ## Remove build and coverage output
	rm -rf $(BIN_DIR) $(COVER_FILE) coverage.html
