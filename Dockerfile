# Use the official Go image as a parent image
FROM golang:1.21 as builder

# Set the working directory
WORKDIR /app

# Install dependencies required for building TDLib
RUN apt-get update && apt-get install -y \
    git \
    cmake \
    g++ \
    make \
    zlib1g-dev \
    libssl-dev \
    gperf \
    && rm -rf /var/lib/apt/lists/*

# Clone and build TDLib
RUN git clone --depth 1 https://github.com/tdlib/td.git /tdlib && \
    mkdir /tdlib/build && \
    cd /tdlib/build && \
    cmake -DCMAKE_BUILD_TYPE=Release -DOPENSSL_ROOT_DIR=/usr .. && \
    cmake --build . --target install

# Copy the local package files to the container's workspace
ADD . /app

# Set environment variables for CGO to find TDLib and system OpenSSL
ENV CGO_CFLAGS="-I/usr/local/include -I/usr/include"
ENV CGO_LDFLAGS="-L/usr/local/lib -L/usr/lib -lssl -lcrypto -ltdjson_static -ltdcore -ltdactor -ltdapi -ltdutils"

# Build the Go app
RUN go mod download && \
    go build -o /app/webBridgeBot .

# Use a smaller image to run the app
FROM debian:bookworm-slim

# Set the working directory
WORKDIR /app

# Copy the binary from the builder image
COPY --from=builder /app/webBridgeBot /app/webBridgeBot

# Copy the run script
COPY run.sh /app/run.sh

# Set the permissions for the binary and the run script
RUN chmod +x /app/webBridgeBot
RUN chmod +x /app/run.sh

# Update and install the necessary libraries
RUN apt-get update && apt-get install -y \
    libssl3 \
    && rm -rf /var/lib/apt/lists/*

# Expose the application's port
EXPOSE 8080

# Run the webBridgeBot binary
CMD ["/app/run.sh"]
