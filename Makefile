# arXiv Cache Manager Makefile

.PHONY: all build test test-verbose test-cover lint fmt vet clean install help

# Default target
all: test build

# Build the binary
build:
	go build -o bin/arxiv ./cmd/arxiv

# Build with race detector
build-race:
	go build -race -o bin/arxiv-race ./cmd/arxiv

# Run all tests
test:
	go test ./... -timeout 120s

# Run tests with verbose output
test-verbose:
	go test -v ./... -timeout 120s

# Run tests with coverage
test-cover:
	go test -cover ./... -timeout 120s

# Generate coverage report
coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Run tests with race detector
test-race:
	go test -race ./... -timeout 120s

# Lint the code
lint:
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run

# Format the code
fmt:
	go fmt ./...

# Run go vet
vet:
	go vet ./...

# Run all checks (fmt, vet, test)
check: fmt vet test

# Clean build artifacts
clean:
	rm -rf bin/
	rm -f coverage.out coverage.html

# Install the binary to $GOPATH/bin
install:
	go install ./cmd/arxiv

# Tidy go.mod
tidy:
	go mod tidy

# Download dependencies
deps:
	go mod download

# Generate embeddings (requires Python)
embeddings:
	@which python3 > /dev/null || (echo "Python3 required for embeddings" && exit 1)
	@test -f tools/requirements.txt && pip3 install -r tools/requirements.txt || true
	python3 tools/generate_embeddings.py

# Build Docker image
docker:
	docker build -t arxiv-cache .

# Run Docker container
docker-run:
	docker run -p 8080:8080 -v $(HOME)/.arxiv:/root/.arxiv arxiv-cache

# Benchmark tests
bench:
	go test -bench=. -benchmem ./...

# Update dependencies
update:
	go get -u ./...
	go mod tidy

# Show help
help:
	@echo "arXiv Cache Manager - Available targets:"
	@echo ""
	@echo "  make build        - Build the binary"
	@echo "  make test         - Run all tests"
	@echo "  make test-verbose - Run tests with verbose output"
	@echo "  make test-cover   - Run tests with coverage"
	@echo "  make coverage     - Generate HTML coverage report"
	@echo "  make lint         - Run golangci-lint"
	@echo "  make fmt          - Format code"
	@echo "  make vet          - Run go vet"
	@echo "  make check        - Run fmt, vet, and test"
	@echo "  make clean        - Remove build artifacts"
	@echo "  make install      - Install binary to GOPATH/bin"
	@echo "  make tidy         - Tidy go.mod"
	@echo "  make deps         - Download dependencies"
	@echo "  make docker       - Build Docker image"
	@echo "  make docker-run   - Run Docker container"
	@echo "  make bench        - Run benchmarks"
	@echo "  make embeddings   - Generate embeddings (requires Python)"
	@echo "  make help         - Show this help"
