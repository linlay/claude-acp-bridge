APP_NAME := proxy-acp
CLAUDE_CLI_ACP_NAME := claude-cli-acp
BIN_DIR := bin
ENV ?= .env

.PHONY: build run test fmt clean

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(APP_NAME) ./cmd/proxy-acp
	go build -o $(BIN_DIR)/$(CLAUDE_CLI_ACP_NAME) ./cmd/claude-cli-acp

run:
	go run ./cmd/proxy-acp -env $(ENV)

test:
	go test ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -rf $(BIN_DIR)
