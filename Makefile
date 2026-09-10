GO_FILES := $(shell find cmd internal -type f -name '*.go' -print)
STATICCHECK_VERSION := 2024.1.1

.PHONY: fmt fmt-check test test-fast test-http test-integration test-acceptance test-container test-race vet staticcheck check tools

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

test-race:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

check: fmt-check test test-race vet staticcheck

tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
