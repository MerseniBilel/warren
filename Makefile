# Warren — multi-module repository. `go test ./...` from the root tests one
# module and exits zero (CLAUDE.md); every target iterates MODULES explicitly.
# Adapter modules are appended here as they are created.
MODULES := . cli transport/http persistence/postgres observability broker/kafka

.PHONY: ci fmt vet lint invariants test bench workspace

ci: workspace fmt vet lint invariants test

# go.work is how a submodule resolves the core module before it is tagged.
# It is GENERATED, never committed: a committed replace or workspace breaks
# `go get` for users, which is invariant 8. Regenerating is idempotent, so
# every target that compiles depends on it.
# NB: `go work init` only — never `go work sync`. Sync writes the workspace's
# resolved versions BACK into every module's go.mod, which quietly added an
# indirect testify require to the CORE module. Invariant 1 is about direct
# dependencies, so the check passed and the contamination would have shipped.
workspace:
	@rm -f go.work go.work.sum
	@go work init $(MODULES)

fmt:
	@for m in $(MODULES); do \
		out=$$(cd $$m && gofmt -l .); \
		if [ -n "$$out" ]; then echo "gofmt: needs formatting in $$m:"; echo "$$out"; exit 1; fi; \
	done

vet: workspace
	@set -e; for m in $(MODULES); do echo "--- vet $$m"; (cd $$m && go vet ./...); done

lint: workspace
	@set -e; for m in $(MODULES); do echo "--- lint $$m"; (cd $$m && golangci-lint run ./...); done

invariants:
	@./scripts/invariants.sh
	@cd cli && go run ./cmd/warren lint arch ..

test: workspace
	@set -e; for m in $(MODULES); do echo "--- test $$m"; (cd $$m && go test -race ./...); done

bench: workspace
	@set -e; for m in $(MODULES); do echo "--- bench $$m"; (cd $$m && go test -run '^$$' -bench . -benchmem ./...); done
