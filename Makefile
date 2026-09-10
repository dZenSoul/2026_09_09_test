GO_FILES := $(shell find cmd internal -type f -name '*.go' -print)
STATICCHECK_VERSION := 2024.1.1

.PHONY: fmt fmt-check test test-fast test-http test-integration test-acceptance test-container test-stack test-load test-race vet staticcheck check tools

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$(shell gofmt -l $(GO_FILES))" || (echo "run 'make fmt' for:"; gofmt -l $(GO_FILES); exit 1)

test:
	go test ./...

test-fast:
	go test ./... -skip '^(TestAcceptanceLifecycle|TestPostgresRepositoriesIntegration|TestRuntimeImageMultipartTemporaryStorage)$$'

test-http:
	go test ./internal/httptransport -skip '^(TestAcceptanceLifecycle|TestRuntimeImageMultipartTemporaryStorage)$$'

test-integration:
	@test -n "$$TEST_POSTGRES_DSN" || (echo "TEST_POSTGRES_DSN is required"; exit 1)
	go test ./internal/repository/postgres -run '^TestPostgresRepositoriesIntegration$$' -count=1
	go test ./internal/httptransport -run '^TestAcceptanceLifecycle$$' -count=1

test-acceptance: test-http test-integration

test-container:
	RUN_DOCKER_MULTIPART_TEST=1 go test ./internal/httptransport -run TestRuntimeImageMultipartTemporaryStorage -count=1

# Black-box check for an already running Compose stack. Override BASE_URL and
# ADMIN_TOKEN when testing anything other than the local demonstration setup.
test-stack:
	ADMIN_TOKEN="$${ADMIN_TOKEN:-local-admin-token-change-me}" go run ./cmd/stacktest -base-url "$${BASE_URL:-http://127.0.0.1:8080}"

test-load:
	ADMIN_TOKEN="$${ADMIN_TOKEN:-local-admin-token-change-me}" go run ./cmd/stacktest -mode load -base-url "$${BASE_URL:-http://127.0.0.1:8080}" -load-profile "$${LOAD_PROFILE:-read}" -load-duration "$${LOAD_DURATION:-30s}" -workers "$${WORKERS:-16}" -max-ops "$${MAX_OPS:-0}"

test-race:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

check: fmt-check test test-race vet staticcheck

tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
