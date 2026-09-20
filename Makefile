# Build, test, and lint for mill.
#
# Tools come from mise.toml. Run `mise install` once; the shims put every tool
# used below on PATH. The only system package assumed is podman, which
# testcontainers and the development stack use.

MODULE  := github.com/ivy/mill
BINARY  := mill

# Version is the nearest v-prefixed tag. A build from exactly that tag on a
# clean tree is the release, `0.1.0`, and nothing else is: any other build
# carries a development marker as semver build metadata — `0.1.0+dev.3.4bd4269`,
# the commits past the tag and the commit, with `.dirty` when the tree has
# local changes — so `mill version` and a package's file name both say what
# they were built from, and rpm orders the build after the tag it grew from.
# Without a tag the build is 0.0.0 with the same marker. `make version`
# prints it.
GIT_DESCRIBE := $(shell git describe --tags --match 'v*' --long --dirty 2>/dev/null)
ifneq ($(GIT_DESCRIBE),)
VERSION ?= $(shell echo '$(GIT_DESCRIBE)' | sed -E 's/^v//; s/-0-g[0-9a-f]+$$//; s/-([0-9]+)-g([0-9a-f]+)$$/+dev.\1.\2/; s/-([0-9]+)-g([0-9a-f]+)-dirty$$/+dev.\1.\2.dirty/')
else
GIT_SHA   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
GIT_DIRTY := $(shell git diff-index --quiet HEAD -- 2>/dev/null || echo .dirty)
VERSION   ?= 0.0.0+dev.0.$(GIT_SHA)$(GIT_DIRTY)
endif

LDFLAGS := -X $(MODULE)/internal/version.version=$(VERSION)

GINKGO := ginkgo -r -p --randomize-all --fail-on-pending --keep-going

MIGRATIONS := internal/store/migrations

COMPOSE := podman compose
# Silence podman's "executing external compose provider" banner.
export PODMAN_COMPOSE_WARNING_LOGS = false

.PHONY: all build version check test test-unit test-db test-acceptance lint lint-contract check-breaking check-generated typecheck-ts generate sidecar-deps sidecar-build clean db-up db-down db-reset db-migrate

all: build

# The gate before a release, and what CI runs: every lint the commit hook
# applies, the contract report it deliberately does not, and every spec.
check: lint lint-contract test

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mill

# The version a build from this tree carries, as `mill version` prints it.
version:
	@echo $(VERSION)

# Every spec. Database specs need podman for testcontainers.
test:
	$(GINKGO) ./...

# Specs that run without external services.
test-unit:
	$(GINKGO) --label-filter='!db' ./...

# Specs that need a real PostgreSQL, labelled "db".
test-db:
	$(GINKGO) --label-filter='db' ./...

# The acceptance suite: the whole mill against real dependencies — a real
# dagger, a real GitLab, the real sidecar, a real Claude account. It costs
# money and spends merge requests, so it is off in `make test` and turned on
# here alone. docs/runbook.md is what it needs, what it leaves behind, and
# how to clean up after it.
#
# The specs are independent and run in parallel, so the suite's wall clock is
# its slowest scenario rather than their sum.
test-acceptance: sidecar-build
	MILL_ACCEPTANCE=1 ginkgo -p --randomize-all --fail-on-pending --timeout=90m ./tests/acceptance

# The gate a commit passes. go vet catches what golangci-lint's analyzers do
# not overlap with; the rules that carry design decisions (depguard,
# forbidigo) live in .golangci.yml. buf lints the proto contracts for style
# and documentation, and sqlc parses the migrations and the queries against
# each other, so a query the schema cannot answer fails the build rather than
# a spec.
#
# Compatibility of the contract is deliberately not here: mill's protos move
# with its design, so a breaking change is a thing a node does on purpose and
# a commit gate that refuses it is the wrong instrument. lint-contract
# reports it instead. See proto/README.md.
lint:
	go vet ./...
	golangci-lint run ./...
	buf lint
	sqlc vet
	$(MAKE) --no-print-directory check-generated
	$(MAKE) --no-print-directory typecheck-ts

# The compatibility report: what this tree's contract would break for a
# client generated from main. It is out of `lint` and so out of the
# pre-commit hook, and in `check`, which is what CI and a person run before a
# release. Run it by hand whenever a proto changes; proto/README.md says when
# breaking it is allowed.
lint-contract:
	@$(MAKE) --no-print-directory check-breaking

# The proto contracts are compared against main, so a change that would
# break a generated client is named. A checkout whose main carries no
# proto/ — before this contract first landed there, or a clone with no main
# branch — has nothing to compare against, and buf treats an empty module as
# an error rather than as "no breaking changes".
check-breaking:
	@if git cat-file -e main:proto 2>/dev/null; then \
	  buf breaking --against '.git#branch=main'; \
	 else \
	  echo "buf breaking: main carries no proto/ to compare against; skipped"; \
	 fi

# Generated code is committed, so a consumer needs no proto toolchain. Both
# templates are regenerated into a scratch tree and compared against this
# one; either lagging its proto fails the build. Specs living beside
# generated packages are hand-written and excluded from the comparison.
check-generated: sidecar-deps
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	  buf generate -o "$$tmp" && \
	  buf generate --template buf.gen.ts.yaml -o "$$tmp" && \
	  if ! diff -r --exclude='*_test.go' "$$tmp/gen" gen || \
	     ! diff -r "$$tmp/sidecar/gen" sidecar/gen; then \
	    echo "lint: generated code is stale relative to proto/; run make generate"; \
	    exit 1; \
	  fi
	@sqlc diff || { \
	  echo "lint: generated queries are stale relative to $(MIGRATIONS) or the query files; run make generate"; \
	  exit 1; \
	 }

# Code generators. Each is deterministic: running twice leaves the tree
# unchanged, which check-generated relies on.
generate: sidecar-deps
	go generate ./...
	buf generate
	buf generate --template buf.gen.ts.yaml
	sqlc generate

# The sidecar is the other end of the harness wire, so the generated
# TypeScript has to type-check for the same reason the generated Go has to
# compile.
typecheck-ts: sidecar-deps
	pnpm --dir sidecar exec tsc --noEmit

# protoc-gen-es is a node package, so the TypeScript half of the contract
# cannot be generated or checked without the sidecar's dependencies. The
# lockfile pins them and pnpm's content-addressable store makes a warm
# install offline and near-instant.
sidecar-deps:
	@pnpm install --dir sidecar --frozen-lockfile --prefer-offline --silent

# The sidecar the worker spawns per skill step, compiled to
# sidecar/dist/main.js. The specs build it themselves; this is for running it
# by hand and for whoever packages a worker.
sidecar-build: sidecar-deps
	pnpm --dir sidecar run build

clean:
	rm -f $(BINARY)
	rm -rf dist sidecar/dist

# Development database. See compose.yaml.

# .env carries the development credentials and is gitignored. Seed it from the
# example on a fresh clone so db-up works out of the box.
.env:
	cp .env.example .env

# Start PostgreSQL and block until it accepts TCP connections.
db-up: .env
	$(COMPOSE) up -d --wait --wait-timeout 30

# Stop the stack. The data volume survives; `podman compose down -v` removes it.
db-down:
	$(COMPOSE) down

# Apply every pending migration to the development database. The service and
# the specs embed the same files; this is for working on the schema by hand.
# DATABASE_URL comes from .env.
db-migrate: db-up
	@set -a && . ./.env && set +a && \
	  goose -dir $(MIGRATIONS) postgres "$$DATABASE_URL" up

# Drop and recreate the development database inside the running container.
# Credentials resolve from the container's environment, so this file never
# needs to read .env.
db-reset: db-up
	$(COMPOSE) exec -T postgres sh -euc '\
	  psql -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d postgres \
	    -c "DROP DATABASE IF EXISTS $$POSTGRES_DB WITH (FORCE)" \
	    -c "CREATE DATABASE $$POSTGRES_DB"'
