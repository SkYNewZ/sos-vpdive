.PHONY: test lint fmt css fixtures build run account

# Build version, shown in logs and Sentry events.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

css:
	./scripts/tailwind.sh

# Regenerate the synthetic workbooks in testdata/fixtures/.
fixtures:
	go test ./internal/members ./internal/payments -run TestFixturesAreUpToDate -update

build: css
	CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/sos-vpdive ./cmd/sos-vpdive

# Local run on http://sos.localhost:8080 and http://comite.localhost:8080.
# Needs .env (copy .env.example, APP_ENV=development).
run: build
	set -a && . ./.env && set +a && DATA_DIR=./data ./bin/sos-vpdive serve

# Create a local account or give it a temporary password: make account ARGS='-name Alice -role Présidente alice'
account: build
	set -a && . ./.env && set +a && DATA_DIR=./data ./bin/sos-vpdive reset-password $(ARGS)
