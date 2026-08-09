.PHONY: fmt vet test check

export CGO_ENABLED ?= 0

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

test:
	go test ./...

check: vet test
