.PHONY: test lint fmt css build run

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

css:
	./scripts/tailwind.sh

build: css
	CGO_ENABLED=0 go build -trimpath -o bin/sos-vpdive ./cmd/sos-vpdive

# Local run on http://sos.localhost:8080 and http://comite.localhost:8080.
# Needs .env (copy .env.example, APP_ENV=development) and admins.yaml.
run: build
	set -a && . ./.env && set +a && ADMINS_FILE=./admins.yaml DATA_DIR=./data ./bin/sos-vpdive serve
