# Makefile for cloning, building TDLib, building OpenSSL, and building a Go application (webBridgeBot) that uses TDLib

# Define variables
TDLIB_GIT_REPO=https://github.com/tdlib/td.git
OPENSSL_GIT_REPO=https://github.com/openssl/openssl.git
OPENSSL_VERSION=openssl-3.0.0
TDLIB_DIR=$(CURDIR)/tdlib
OPENSSL_DIR=$(CURDIR)/openssl
BUILD_DIR=$(TDLIB_DIR)/build
TDLIB_INSTALL_DIR=$(CURDIR)/tdlib_install
OPENSSL_INSTALL_DIR=$(CURDIR)/openssl_install

# Environment variables for Go build
export CGO_CFLAGS=-I$(TDLIB_INSTALL_DIR)/include -I$(OPENSSL_INSTALL_DIR)/include
export CGO_LDFLAGS=-L$(TDLIB_INSTALL_DIR)/lib -L$(OPENSSL_INSTALL_DIR)/lib -lssl -lcrypto

# Default target builds OpenSSL, TDLib, and the Go application
all: openssl tdlib webBridgeBot

# Clone and build OpenSSL
openssl:
	git clone --depth 1 --branch $(OPENSSL_VERSION) $(OPENSSL_GIT_REPO) $(OPENSSL_DIR) && \
	cd $(OPENSSL_DIR) && \
	./config --prefix=$(OPENSSL_INSTALL_DIR) && \
	make && make install

# Clone TDLib repository
$(TDLIB_DIR):
	git clone --depth 1 $(TDLIB_GIT_REPO) $(TDLIB_DIR)

# Build TDLib
tdlib: openssl $(TDLIB_DIR)
	mkdir -p $(BUILD_DIR) && cd $(BUILD_DIR) && \
	cmake -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=$(INSTALL_DIR) -DOPENSSL_ROOT_DIR=$(OPENSSL_INSTALL_DIR) .. && \
	cmake --build . --target install

# Build the Go application webBridgeBot
webBridgeBot: tdlib
	go build -o webBridgeBot .

# Clean up build and cloned directories, and remove webBridgeBot binary
clean:
	rm -rf $(BUILD_DIR)
	rm -rf $(TDLIB_DIR)
	rm -rf $(OPENSSL_DIR)
	rm -rf $(INSTALL_DIR)
	rm -rf $(OPENSSL_INSTALL_DIR)
	rm -f webBridgeBot

# Phony targets
.PHONY: all clean tdlib webBridgeBot openssl
