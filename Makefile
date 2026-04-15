.PHONY: test test-race test-stress lint

COUNT ?= 1

# Run tests
# Usage:
#   make test          # default 1 iteration
#   make test COUNT=50 # custom iteration count
test:
	go test -v -count=$(COUNT) ./...

# Run tests with race detector
# Usage:
#   make test-race            # default 1 iteration
#   make test-race COUNT=100  # custom iteration count
test-race:
	go test -v -race -count=$(COUNT) ./...

# Run goimports and golangci-lint
lint:
	goimports -format-only -w -local github.com/ngrok-oss/tableroll .
	golangci-lint run ./...
