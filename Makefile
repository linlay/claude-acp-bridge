APP_NAME := proxy-acp
CONFIG ?= config.yml

.PHONY: build run test fmt clean

build:
	go build -o $(APP_NAME) ./cmd/proxy-acp

run:
	go run ./cmd/proxy-acp -config $(CONFIG)

test:
	go test ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -f $(APP_NAME)
