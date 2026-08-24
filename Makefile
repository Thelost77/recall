BUILD_DIR := bin
PREFIX ?= $(HOME)/.local
INSTALL_DIR ?= $(PREFIX)/bin
BINARIES := recall agent-sessions

.PHONY: build test check install clean

build:
	go build -o $(BUILD_DIR)/recall ./cmd/recall
	go build -o $(BUILD_DIR)/agent-sessions ./cmd/agent-sessions

test:
	go test ./...

check:
	go test ./...
	go test -race ./...
	go vet ./...

install: build
	install -d -m 0755 $(INSTALL_DIR)
	install -m 0755 $(BUILD_DIR)/recall $(INSTALL_DIR)/recall
	install -m 0755 $(BUILD_DIR)/agent-sessions $(INSTALL_DIR)/agent-sessions

clean:
	rm -rf $(BUILD_DIR)
