.PHONY: build test vet fmt tidy

BINARY := bin/kubectl-coldstart

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BINARY) ./cmd/coldstart

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal rules

tidy:
	go mod tidy
