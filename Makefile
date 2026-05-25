APP_NAME := proxy-acp
CLAUDE_CLI_ACP_NAME := claude-cli-acp
ENV ?= .env

.PHONY: build run test fmt clean

build:
	go build -o $(APP_NAME) ./cmd/proxy-acp
	go build -o $(CLAUDE_CLI_ACP_NAME) ./cmd/claude-cli-acp

run:
	go run ./cmd/proxy-acp -env $(ENV)

test:
	go test ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -f $(APP_NAME) $(CLAUDE_CLI_ACP_NAME)
