BINARY := mqtt-raspberry-controller
PACKAGE := ./cmd/$(BINARY)
BIN_DIR := bin
VERSION ?= dev
STATICCHECK_VERSION ?= v0.8.1
GOVULNCHECK_VERSION ?= v1.8.0
export VERSION STATICCHECK_VERSION GOVULNCHECK_VERSION

.PHONY: build version test test-race coverage fmt fmt-check vet staticcheck govulncheck tidy-check packaging-check check ci release release-verify clean

build:
	mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w -X main.version=$$VERSION" -o $(BIN_DIR)/$(BINARY) $(PACKAGE)

version: build
	@$(BIN_DIR)/$(BINARY) --version

test:
	go test ./...

test-race:
	go test -race ./...

coverage:
	mkdir -p $(BIN_DIR)
	go test -coverprofile=$(BIN_DIR)/coverage.out ./...
	go tool cover -func=$(BIN_DIR)/coverage.out

fmt:
	go fmt ./...

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		printf 'Files not formatted with gofmt:\n%s\n' "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@$$STATICCHECK_VERSION ./...

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$$GOVULNCHECK_VERSION ./...

tidy-check:
	go mod tidy -diff

packaging-check:
	./packaging/tests/verify-install.sh
	./packaging/tests/install-script.sh
	./packaging/tests/uninstall-script.sh

check: build fmt-check test vet tidy-check packaging-check

ci: fmt-check tidy-check vet test test-race staticcheck govulncheck packaging-check

release:
	@test "$$VERSION" != dev || (printf 'set VERSION to a SemVer release value\n' >&2; exit 2)
	./scripts/build-release.sh "$$VERSION"

release-verify:
	@test "$$VERSION" != dev || (printf 'set VERSION to the built SemVer release value\n' >&2; exit 2)
	./scripts/verify-release.sh "$$VERSION"

clean:
	rm -rf $(BIN_DIR) dist
