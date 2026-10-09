APP_NAME := claude-acp-bridge
BIN_DIR := bin
DIST_DIR := dist
RELEASE_DIR := $(DIST_DIR)/release
VERSION ?= v0.1.1
OS := darwin
ARCH ?= arm64
ASSET_NAME := $(APP_NAME)-$(VERSION)-$(OS)-$(ARCH).zip
ENV ?= .env

.PHONY: build build-windows-amd64 release-plugin release-program run test fmt clean

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(APP_NAME) ./cmd/claude-acp-bridge

build-windows-amd64:
	mkdir -p $(DIST_DIR)/windows-amd64
	GOOS=windows GOARCH=amd64 go build -o $(DIST_DIR)/windows-amd64/$(APP_NAME).exe ./cmd/claude-acp-bridge

# Desktop imports a single-root ZIP; release builds explicitly target macOS.
release-program: release-plugin

release-plugin:
	@test "$(OS)" = darwin || (echo "Desktop plugin release currently supports macOS only"; exit 1)
	@test "$(ARCH)" = arm64 -o "$(ARCH)" = amd64 || (echo "unsupported macOS architecture"; exit 1)
	mkdir -p $(DIST_DIR)/bundle/$(APP_NAME)/backend $(RELEASE_DIR)
	GOOS=darwin GOARCH=$(ARCH) go build -o $(DIST_DIR)/bundle/$(APP_NAME)/backend/$(APP_NAME) ./cmd/$(APP_NAME)
	python3 scripts/package-plugin.py --version "$(VERSION)" --arch "$(ARCH)" --output "$(RELEASE_DIR)/$(ASSET_NAME)"

run:
	go run ./cmd/claude-acp-bridge -env $(ENV)

test:
	go test ./...

fmt:
	gofmt -w cmd internal

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR)
