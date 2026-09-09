GO_FILES := $(shell find cmd internal -type f -name '*.go' -print)
STATICCHECK_VERSION := 2024.1.1

.PHONY: fmt fmt-check test test-race vet staticcheck check tools

fmt:
	gofmt -w $(GO_FILES)

fmt-check:
	@test -z "$(shell gofmt -l $(GO_FILES))" || (echo "run 'make fmt' for:"; gofmt -l $(GO_FILES); exit 1)

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

staticcheck:
	staticcheck ./...

check: fmt-check test test-race vet staticcheck

tools:
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
