.PHONY: fmt vet test integration-test check run migrate-up migrate-down migrate-version migrate-check

export CGO_ENABLED ?= 0

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

test:
	go test ./...

integration-test:
	test -n "$$TEST_DATABASE_URL"
	go test -count=1 ./internal/store/postgres

check: vet test

run:
	go run ./cmd/api

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down

migrate-version:
	go run ./cmd/migrate version

migrate-check:
	go run ./cmd/migrate check
