# bitcal-api — build and test.
#
# Two things about this build are not optional, and both used to be recorded
# only in the Dockerfile that this file replaces:
#
#   1. -tags fts5. gorm.io/driver/sqlite uses mattn/go-sqlite3, which does NOT
#      compile FTS5 in by default. Without the tag the binary builds and starts
#      quite happily, and then every query touching events_fts fails at runtime
#      with "no such module: fts5" — so /api/search returns 500 and nothing
#      else complains. CGO_ENABLED=1 is required for the same driver.
#
#   2. Build on Ubuntu/glibc, or in a container matching the target. CGO ties
#      the binary to the C library it was built against. The deployment target
#      is Ubuntu 24.04 (glibc); a binary built on Alpine (musl) or on a Mac
#      will not start there at all. `make build-ubuntu` does this in Docker if
#      you are not already on a matching host.

BINARY  ?= bitcal-api
VERSION ?= 0.1.0-$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GO      ?= go

BUILD_ENV   = CGO_ENABLED=1
BUILD_FLAGS = -tags fts5 -ldflags "-X main.version=$(VERSION)"

.PHONY: build test vet fmt clean build-ubuntu version

build:
	$(BUILD_ENV) $(GO) build $(BUILD_FLAGS) -o $(BINARY) .

# The suite lives in tests/ and is black-box: it builds this binary, starts it
# against a fixture staged at 0444 in a 0555 directory, and drives it over
# HTTP. That means `make test` also proves the build itself, including the tag.
test:
	$(BUILD_ENV) $(GO) test -tags fts5 ./...

vet:
	$(BUILD_ENV) $(GO) vet -tags fts5 ./...

fmt:
	$(GO) fmt ./...

version:
	@echo $(VERSION)

clean:
	rm -f $(BINARY)

# Produce a binary that will actually run on the box, from any host with
# Docker. Same build line, glibc userland.
build-ubuntu:
	docker run --rm -v "$(CURDIR)":/src -w /src golang:1.23-bookworm \
		sh -c 'apt-get update >/dev/null && apt-get install -y --no-install-recommends gcc libc6-dev >/dev/null && \
		       $(BUILD_ENV) go build $(BUILD_FLAGS) -o $(BINARY) .'
