DB_URL ?= flowbee.db
export FLOWBEE_DATABASE_URL := $(DB_URL)

.PHONY: build tidy migrate serve seed fmt archcheck laddercheck lint test accept clean

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

laddercheck:
	go run ./tools/laddercheck

lint: archcheck laddercheck
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed; skipping"

test:
	go test ./... -short -race

accept:
	go test ./test/... -race

clean:
	rm -f flowbee.db flowbee.db-wal flowbee.db-shm
	rm -rf bin
