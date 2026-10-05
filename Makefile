.PHONY: test lint fmt css

test:
	go test -race ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

css:
	./scripts/tailwind.sh
