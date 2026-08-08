.PHONY: build test test-verbose cover cover-html vet lint tidy clean help

COVERAGE_OUT := coverage.out
COVERAGE_HTML := coverage.html

help:
	@echo "Available targets:"
	@echo "  build        Build the package"
	@echo "  vet          Run go vet"
	@echo "  lint         Run golangci-lint"
	@echo "  test         Run tests (Mongo-backed tests skip if Docker is unavailable)"
	@echo "  test-verbose Run tests with -v"
	@echo "  cover        Run tests and print per-function coverage"
	@echo "  cover-html   Run tests and open an HTML coverage report"
	@echo "  tidy         Run go mod tidy"
	@echo "  clean        Remove generated coverage files"

build:
	go build ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test: vet
	go test ./...

test-verbose: vet
	go test ./... -v

cover: vet
	go test ./... -coverprofile=$(COVERAGE_OUT)
	go tool cover -func=$(COVERAGE_OUT)

cover-html: cover
	go tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	@echo "Coverage report written to $(COVERAGE_HTML)"
	@open $(COVERAGE_HTML) 2>/dev/null || echo "Open $(COVERAGE_HTML) in your browser to view it"

tidy:
	go mod tidy

clean:
	rm -f $(COVERAGE_OUT) $(COVERAGE_HTML)
