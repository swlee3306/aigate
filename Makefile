.PHONY: build test e2e

build:
	go build -o bin/aigate ./cmd/aigate

test:
	go test ./...

e2e:
	go test ./e2e -count=1
