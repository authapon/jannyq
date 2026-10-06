VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet fmt run docker clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/jannyq ./cmd/jannyq

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

# Chat in the terminal. Example: make run MODEL=qwen3:8b
run:
	go run ./cmd/jannyq --cli --llm-model $(MODEL)

docker:
	docker build --build-arg VERSION=$(VERSION) -t jannyq:latest .

clean:
	rm -rf bin
