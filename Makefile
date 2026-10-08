BIN     ?= bin/feedreader
PKG     := github.com/giacomomicoli/feedreader
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/config.Version=$(VERSION)

.PHONY: run build test vet fmt-check tidy check clean

## run: start the app on 127.0.0.1:8080 with its database in ./data
run:
	FR_DATA_DIR=./data go run -ldflags "-X $(PKG)/internal/config.Version=$(VERSION)" ./cmd/feedreader

## build: static, CGO-free binary in $(BIN)
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/feedreader

## test: all tests with the race detector
test:
	go test -race ./...

vet:
	go vet ./...

## fmt-check: fail if any Go file is not gofmt-formatted
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on:"; echo "$$unformatted"; exit 1; \
	fi

tidy:
	go mod tidy

## check: what CI runs before building
check: fmt-check vet test

clean:
	rm -rf bin
