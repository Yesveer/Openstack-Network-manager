BINARY     := openstack-network-manager
VERSION    ?= 1.0.0
GOOS       := linux
GOARCH     := amd64
BUILD_DIR  := dist
DEB_DIR    := $(BUILD_DIR)/deb-build

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build deb clean test vet

all: build

## build: compile the binary for linux/amd64
build:
	@echo "==> Building $(BINARY) v$(VERSION)"
	@mkdir -p $(BUILD_DIR)
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags="$(LDFLAGS)" \
		-o $(BUILD_DIR)/$(BINARY) ./cmd/$(BINARY)
	@echo "    Binary: $(BUILD_DIR)/$(BINARY)"

## deb: build binary then package as a .deb
deb: build
	@echo "==> Packaging $(BINARY)_$(VERSION)_$(GOARCH).deb"
	@bash scripts/build-deb.sh $(VERSION)

## test: run unit tests
test:
	go test ./...

## vet: run go vet
vet:
	go vet ./...

## clean: remove build artefacts
clean:
	rm -rf $(BUILD_DIR)

## help: list targets
help:
	@grep -E '^##' Makefile | sed 's/## /  /'
