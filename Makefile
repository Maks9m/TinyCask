.DEFAULT_GOAL := check

GO       ?= go
GOLANGCI_LINT ?= golangci-lint
FUZZTIME ?= 30s

# Fuzz targets as "<FuzzName>:<package>". A target whose package does not
# define it yet is skipped, so `make fuzz` works from the first ticket on.
FUZZ_TARGETS := FuzzDecodeRecord:./internal/storage/ FuzzParseFrame:./internal/server/

.PHONY: build test lint cover fuzz bench run check

build:
	$(GO) build ./...
	$(GO) build -o bin/tinycask ./cmd/tinycask

test:
	$(GO) test -race -count=1 ./...

lint:
	$(GOLANGCI_LINT) run ./...

cover:
	$(GO) test -race -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

fuzz:
	@set -e; for t in $(FUZZ_TARGETS); do \
		name=$${t%%:*}; pkg=$${t#*:}; \
		list=$$($(GO) test -list "^$$name$$" $$pkg) || exit 1; \
		if printf '%s\n' "$$list" | grep -qx "$$name"; then \
			echo "fuzz $$name in $$pkg for $(FUZZTIME)"; \
			$(GO) test -run '^$$' -fuzz "^$$name$$" -fuzztime $(FUZZTIME) $$pkg; \
		else \
			echo "fuzz $$name: not defined in $$pkg yet, skipping"; \
		fi; \
	done

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/engine/

run:
	$(GO) run ./cmd/tinycask -dir ./data

check: build
	$(GO) vet ./...
	$(MAKE) lint
	$(MAKE) test
