.PHONY: build test test-integration lint up down logs smoke cover

BIN := bin

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/api ./cmd/api
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/worker ./cmd/worker

test:
	go test -race -count=1 ./...

# Requires a running Postgres, e.g. `docker compose up -d postgres`.
test-integration:
	TEST_POSTGRES_DSN=$${TEST_POSTGRES_DSN:-postgres://docs:docs@localhost:5432/docs?sslmode=disable} \
		go test -race -count=1 -tags integration ./internal/storage/...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	golangci-lint run ./...

up:
	docker compose up -d --build --wait

down:
	docker compose down -v

logs:
	docker compose logs -f api worker

smoke:
	bash scripts/smoke.sh
