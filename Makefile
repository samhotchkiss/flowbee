DB_URL ?= postgres://flowbee:flowbee@localhost:5433/flowbee?sslmode=disable
export FLOWBEE_DATABASE_URL := $(DB_URL)

.PHONY: dev down build tidy migrate serve seed fmt archcheck lint test accept

dev: ## start local postgres and wait for health
	docker compose up -d --wait

down:
	docker compose down -v

build:
	CGO_ENABLED=0 go build -o bin/flowbee ./cmd/flowbee

tidy:
	go mod tidy

migrate: build
	./bin/flowbee migrate up

serve: build
	./bin/flowbee serve

seed: build
	./bin/flowbee seed

fmt:
	gofmt -w .

archcheck:
	go run ./tools/archcheck

lint: archcheck
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed; skipping"

test:
	go test ./... -short -race

accept:
	go test ./test/... -race
