# Warren — multi-module repository. `go test ./...` from the root tests one
# module and exits zero (CLAUDE.md); every target iterates MODULES explicitly.
# Adapter modules are appended here as they are created.
MODULES := . cli

.PHONY: ci fmt vet lint invariants test bench

ci: fmt vet lint invariants test

fmt:
	@for m in $(MODULES); do \
		out=$$(cd $$m && gofmt -l .); \
		if [ -n "$$out" ]; then echo "gofmt: needs formatting in $$m:"; echo "$$out"; exit 1; fi; \
	done

vet:
	@set -e; for m in $(MODULES); do echo "--- vet $$m"; (cd $$m && go vet ./...); done

lint:
	@set -e; for m in $(MODULES); do echo "--- lint $$m"; (cd $$m && golangci-lint run ./...); done

invariants:
	@./scripts/invariants.sh
	@cd cli && go run ./cmd/warren lint arch ..

test:
	@set -e; for m in $(MODULES); do echo "--- test $$m"; (cd $$m && go test -race ./...); done

bench:
	@set -e; for m in $(MODULES); do echo "--- bench $$m"; (cd $$m && go test -run '^$$' -bench . -benchmem ./...); done
