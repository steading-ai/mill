# Build, test, and lint for mill.
#
# Tools come from mise.toml. Run `mise install` once; the shims put every tool
# used below on PATH. The only system package assumed is podman, which
# testcontainers and the development stack use.

MODULE  := github.com/ivy/mill
BINARY  := mill

# Version is the nearest v-prefixed tag, marked dirty when the tree has local
# changes. Without a tag the build is a 0.0.0 pre-release carrying the commit,
# so the string is always semver-shaped.
GIT_DESCRIBE := $(shell git describe --tags --match 'v*' --dirty 2>/dev/null | sed 's/^v//')
GIT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
VERSION      ?= $(if $(GIT_DESCRIBE),$(GIT_DESCRIBE),0.0.0-dev+$(GIT_SHA))

LDFLAGS := -X $(MODULE)/internal/version.version=$(VERSION)

GINKGO := ginkgo -r -p --randomize-all --fail-on-pending --keep-going

MIGRATIONS := internal/store/migrations

COMPOSE := podman compose
# Silence podman's "executing external compose provider" banner.
export PODMAN_COMPOSE_WARNING_LOGS = false

.PHONY: all build test test-unit test-db lint generate clean db-up db-down db-reset db-migrate

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/mill

# Every spec. Database specs need podman for testcontainers.
test:
	$(GINKGO) ./...

# Specs that run without external services.
test-unit:
	$(GINKGO) --label-filter='!db' ./...

# Specs that need a real PostgreSQL, labelled "db".
test-db:
	$(GINKGO) --label-filter='db' ./...

# go vet catches what golangci-lint's analyzers do not overlap with; the
# rules that carry design decisions (depguard, forbidigo) live in
# .golangci.yml. buf and sqlc checks join here when proto/ and the queries
# arrive.
lint:
	go vet ./...
	golangci-lint run ./...

# Code generators. Each is deterministic: running twice leaves the tree
# unchanged.
generate:
	go generate ./...

clean:
	rm -f $(BINARY)
	rm -rf dist

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
# DATABASE_URL comes from .env. Until the store node lands there are no
# migration files, and goose treats an empty directory as an error, so an
# empty schema is reported and skipped rather than failing the target.
db-migrate: db-up
	@set -a && . ./.env && set +a && \
	  if ls $(MIGRATIONS)/*.sql >/dev/null 2>&1; then \
	    goose -dir $(MIGRATIONS) postgres "$$DATABASE_URL" up; \
	  else \
	    echo "db-migrate: no migrations in $(MIGRATIONS) yet; nothing to apply"; \
	  fi

# Drop and recreate the development database inside the running container.
# Credentials resolve from the container's environment, so this file never
# needs to read .env.
db-reset: db-up
	$(COMPOSE) exec -T postgres sh -euc '\
	  psql -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d postgres \
	    -c "DROP DATABASE IF EXISTS $$POSTGRES_DB WITH (FORCE)" \
	    -c "CREATE DATABASE $$POSTGRES_DB"'
