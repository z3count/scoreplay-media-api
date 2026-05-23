.PHONY: run build test test-unit test-integration test-coverage lint clean docker-up docker-down

# Default target
all: lint test build

# Run the application locally (requires PostgreSQL to be running)
# Uses text logging for readability; production defaults to JSON.
run: docker-up
	LOG_FORMAT=text go run ./cmd/api/

# Build the binary
build:
	go build -o bin/media-api ./cmd/api/

# Run all tests with race detector
test:
	go test -race -count=1 ./...

# Run unit tests only (no database required)
test-unit:
	go test -race -count=1 -short ./...

# Run integration tests (requires Docker for testcontainers)
test-integration:
	go test -race -count=1 -run Integration ./...

# Run tests with coverage report
test-coverage:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Lint the codebase
lint:
	go vet ./...
	@which staticcheck > /dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipping"

# Clean build artifacts and uploads
clean:
	rm -rf bin/ coverage.out coverage.html

# Start PostgreSQL via Docker Compose
docker-up:
	docker compose up -d

# Stop PostgreSQL
docker-down:
	docker compose down

# Stop PostgreSQL and delete data
docker-reset:
	docker compose down -v
