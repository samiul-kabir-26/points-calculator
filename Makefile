export GOTOOLCHAIN := local

TEST_DATABASE_URL ?= postgres://points:points@localhost:5432/points?sslmode=disable
TEST_REDIS_URL ?= redis://localhost:6379/15

.PHONY: build run test test-integration deps-up deps-down up vet fmt seed-tenants

build:
	go build -o bin/api ./cmd/api

run:
	set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/api

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

# Unit tests only; integration tests skip without TEST_DATABASE_URL / TEST_REDIS_URL.
test:
	go test ./...

deps-up:
	docker compose up -d --wait postgres redis

deps-down:
	docker compose down

test-integration: deps-up
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" TEST_REDIS_URL="$(TEST_REDIS_URL)" go test -race -count=1 ./...

up:
	docker compose up --build

# Creates (or updates) the Daraz, Shopee and Lazada demo tenants on a running API and saves their
# API keys to seeds/tenant_keys.local.json.
seed-tenants:
	set -a; [ -f .env ] && . ./.env; set +a; \
	for f in daraz shopee lazada; do \
		go run ./cmd/seed -url "$${SEED_URL:-http://localhost:8080}" -keys-file seeds/tenant_keys.local.json -file seeds/$$f.json || exit 1; \
	done
