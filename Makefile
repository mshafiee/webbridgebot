# Makefile for cloning, building TDLib and building a Go application (webBridgeBot) that uses TDLib

# Define variables
TDLIB_GIT_REPO=https://github.com/tdlib/td.git
TDLIB_DIR=$(CURDIR)/tdlib
BUILD_DIR=$(TDLIB_DIR)/build
INSTALL_DIR=$(CURDIR)/tdlib_install
OPENSSL_DIR=/opt/homebrew/Cellar/openssl@3/3.2.1

# Environment variables for Go build
export CGO_CFLAGS=-I$(INSTALL_DIR)/include -I$(OPENSSL_DIR)/include
export CGO_LDFLAGS=-L$(INSTALL_DIR)/lib -L$(OPENSSL_DIR)/lib -lssl -lcrypto

# Default target builds both TDLib and the Go application
all: tdlib webBridgeBot

# Clone TDLib repository
$(TDLIB_DIR):
	git clone --depth 1 $(TDLIB_GIT_REPO) $(TDLIB_DIR)

# Build TDLib
tdlib: $(TDLIB_DIR)
	mkdir -p $(BUILD_DIR) && cd $(BUILD_DIR) && \
	cmake -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=$(INSTALL_DIR) .. && \
	cmake --build . --target install

# Build the Go application webBridgeBot
webBridgeBot: tdlib
	go build -o webBridgeBot .

# Clean up build and cloned directories, and remove webBridgeBot binary
clean:
	rm -rf $(BUILD_DIR)
	rm -rf $(TDLIB_DIR)
	rm -rf $(INSTALL_DIR)
	rm -f webBridgeBot

# Phony targets
.PHONY: all clean tdlib webBridgeBot
