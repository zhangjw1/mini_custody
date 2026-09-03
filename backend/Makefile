.PHONY: fmt vet test integration-test e2e-test erc20-preflight check run migrate-up migrate-down migrate-version migrate-check

export CGO_ENABLED ?= 0
export CONFIG_FILE ?= ../deploy/config.local.yaml

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

test:
	go test ./...

integration-test:
	test -n "$$TEST_DATABASE_URL"
	go test -count=1 ./internal/store/postgres

e2e-test:
	E2E_SEPOLIA=1 go test -count=1 -v ./e2e

erc20-preflight:
	go run ./cmd/erc20-preflight

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
