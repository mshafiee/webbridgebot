# Makefile for cloning, building TDLib, building OpenSSL, and building a Go application (webBridgeBot) that uses TDLib

# Define variables
DOCKER_IMAGE_NAME=webbridgebot
DOCKER_TAG=latest
DOCKER_USERNAME=mshafiee

# Default target builds OpenSSL, TDLib, the Go application, and the Docker image
all: webBridgeBot docker

# Build the Go application webBridgeBot
webBridgeBot:
	go build -o webBridgeBot .

# Build Docker image
docker:
	docker buildx create --use
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(DOCKER_USERNAME)/$(DOCKER_IMAGE_NAME):$(DOCKER_TAG) \
		--push . \
		--cache-from=type=registry,ref=$(DOCKER_USERNAME)/$(DOCKER_IMAGE_NAME):cache \
		--cache-to=type=registry,ref=$(DOCKER_USERNAME)/$(DOCKER_IMAGE_NAME):cache,mode=max

# Clean up build and cloned directories, and remove webBridgeBot binary
clean:
	rm -rf $(BUILD_DIR)
	rm -f webBridgeBot

# Phony targets
.PHONY: all clean webBridgeBot docker
