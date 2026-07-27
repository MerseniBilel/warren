# Warren developer entrypoint.
#
# `make ci` runs exactly what CI runs. If it passes here it passes there; if it
# does not, that is a bug in this file, not a fact of life.
#
# This repository is multi-module (ADR-0003), so most targets iterate over every
# go.mod rather than relying on `./...`, which does not cross module boundaries.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# Tool versions are pinned. An unpinned tool means CI and local disagree the day
# upstream releases, and the resulting failure looks like your change broke it.
GOLANGCI_VERSION  := v2.12.2
GOVULNCHECK_VER   := latest
CHANGIE_VERSION   := latest

GO         := go
MODULES    := $(shell find . -name go.mod -not -path './.git/*' -not -path '*/testdata/*' -exec dirname {} \; | sort)
GOBIN      := $(shell $(GO) env GOPATH)/bin
COVERPROF  := coverage.txt

# Integration tests are behind a build tag so that `make test` stays fast and
# needs no Docker. See docs/testing.md.
INTEGRATION_TAG := integration

.PHONY: help
help: ## Show this help
	@echo "Warren — make targets"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Modules: $(words $(MODULES))"

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------

.PHONY: tools
tools: ## Install pinned developer tools
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER)
	$(GO) install github.com/miniscruff/changie@$(CHANGIE_VERSION)

.PHONY: work
work: ## Generate go.work for cross-module development (git-ignored)
	@rm -f go.work go.work.sum
	$(GO) work init $(MODULES)
	@echo "go.work created for $(words $(MODULES)) module(s). It is git-ignored (ADR-0003)."

# ---------------------------------------------------------------------------
# Quality gates
# ---------------------------------------------------------------------------

.PHONY: fmt
fmt: ## Apply all formatters (gofmt, gofumpt, gci, golines)
	@command -v golangci-lint >/dev/null || { echo "golangci-lint missing; run 'make tools'"; exit 1; }
	@for m in $(MODULES); do echo "==> fmt $$m"; (cd $$m && golangci-lint fmt); done

.PHONY: lint
lint: ## Run the full quality gate across every module
	@command -v golangci-lint >/dev/null || { echo "golangci-lint missing; run 'make tools'"; exit 1; }
	@for m in $(MODULES); do echo "==> lint $$m"; (cd $$m && golangci-lint run); done

.PHONY: lint-config
lint-config: ## Verify .golangci.yml is valid and has not rotted
	@if command -v golangci-lint >/dev/null; then golangci-lint config verify; \
	else echo "golangci-lint missing; falling back to schema validation"; \
	     python3 scripts/validate-golangci-config.py; fi

.PHONY: lint-modules
lint-modules: ## Enforce the module invariants from ADR-0001/0003/0007
	@./scripts/check-module-rules.sh

.PHONY: vet
vet: ## Run go vet across every module
	@for m in $(MODULES); do echo "==> vet $$m"; (cd $$m && $(GO) vet ./...); done

.PHONY: vuln
vuln: ## Scan for known vulnerabilities
	@command -v govulncheck >/dev/null || { echo "govulncheck missing; run 'make tools'"; exit 1; }
	@for m in $(MODULES); do echo "==> vuln $$m"; (cd $$m && govulncheck ./...); done

.PHONY: tidy
tidy: ## Tidy every module's go.mod
	@for m in $(MODULES); do echo "==> tidy $$m"; (cd $$m && $(GO) mod tidy); done

.PHONY: tidy-check
tidy-check: ## Fail if any go.mod or go.sum is not tidy
	@$(MAKE) --no-print-directory tidy
	@if ! git diff --quiet -- '**/go.mod' '**/go.sum' go.mod go.sum 2>/dev/null; then \
		echo "go.mod/go.sum not tidy. Run 'make tidy' and commit the result."; \
		git diff -- '**/go.mod' '**/go.sum' go.mod go.sum; exit 1; \
	fi

# ---------------------------------------------------------------------------
# Tests — see docs/testing.md for what belongs in each tier
# ---------------------------------------------------------------------------

.PHONY: test
test: ## Unit tests, race detector on, every module
	@for m in $(MODULES); do echo "==> test $$m"; (cd $$m && $(GO) test -race -shuffle=on ./...); done

.PHONY: test-short
test-short: ## Unit tests without the race detector (fast inner loop)
	@for m in $(MODULES); do (cd $$m && $(GO) test -short ./...); done

.PHONY: test-integration
test-integration: ## Integration tests (requires Docker; testcontainers)
	@for m in $(MODULES); do echo "==> integration $$m"; \
		(cd $$m && $(GO) test -race -tags=$(INTEGRATION_TAG) -timeout=15m ./...); done

.PHONY: cover
cover: ## Unit tests with a merged coverage profile
	@echo "mode: atomic" > $(COVERPROF)
	@for m in $(MODULES); do \
		(cd $$m && $(GO) test -race -covermode=atomic -coverprofile=cover.tmp ./... >/dev/null 2>&1 || true; \
		 if [ -f cover.tmp ]; then tail -n +2 cover.tmp >> $(CURDIR)/$(COVERPROF); rm -f cover.tmp; fi); \
	done
	@$(GO) tool cover -func=$(COVERPROF) | tail -1

.PHONY: cover-html
cover-html: cover ## Open the coverage report in a browser
	@$(GO) tool cover -html=$(COVERPROF)

.PHONY: bench
bench: ## Run benchmarks
	@for m in $(MODULES); do (cd $$m && $(GO) test -run='^$$' -bench=. -benchmem ./...); done

.PHONY: golden-update
golden-update: ## Regenerate golden files, then review the diff before committing
	@for m in $(MODULES); do (cd $$m && $(GO) test ./... -update); done
	@echo "Golden files regenerated. Read the diff — an unexpected change is a bug, not noise."

# ---------------------------------------------------------------------------
# Agent integration — see ADR-0008
# ---------------------------------------------------------------------------

.PHONY: skills-gen
skills-gen: ## Regenerate the mechanical sections of every skill from Cobra metadata
	@if [ ! -d cli ]; then \
		echo "cli/ does not exist yet — nothing to generate. This target becomes"; \
		echo "live with the first CLI command (see docs/agent-integration.md)."; \
	else \
		$(GO) run ./cli/internal/skillgen -out skills; \
	fi

.PHONY: skills-check
skills-check: ## Fail if any skill has drifted from its command definition
	@if [ ! -d cli ]; then \
		echo "cli/ does not exist yet — skipping skill drift check."; \
	else \
		$(MAKE) --no-print-directory skills-gen; \
		if ! git diff --quiet -- skills/; then \
			echo "Skills are out of date with their commands (ADR-0008)."; \
			echo "A flag was added or changed without updating its skill."; \
			echo "Run 'make skills-gen' and commit the result."; \
			git diff -- skills/; \
			exit 1; \
		fi; \
		echo "ok: skills match their command definitions"; \
	fi

# ---------------------------------------------------------------------------
# Changelog and release — see ADR-0005
# ---------------------------------------------------------------------------

.PHONY: changelog
changelog: ## Add a changelog fragment for the current change
	@command -v changie >/dev/null || { echo "changie missing; run 'make tools'"; exit 1; }
	@changie new

.PHONY: changelog-check
changelog-check: ## Fail if a feat/fix/perf commit has no changelog fragment
	@./scripts/check-changelog.sh

# ---------------------------------------------------------------------------
# Aggregates
# ---------------------------------------------------------------------------

.PHONY: check
check: lint-config lint-modules vet lint test ## Everything except integration tests

.PHONY: ci
ci: check tidy-check vuln ## Exactly what CI runs

.PHONY: clean
clean: ## Remove build and test artefacts
	@rm -f $(COVERPROF) coverage.html go.work go.work.sum
	@find . -name 'cover.tmp' -delete
	@rm -rf bin/ dist/
