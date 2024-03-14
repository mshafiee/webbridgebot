# Use the official Go image as a parent image
FROM golang:1.18 as builder

# Set the working directory
WORKDIR /app

# Install dependencies required for building TDLib and OpenSSL
RUN apt-get update && apt-get install -y \
    git \
    cmake \
    g++ \
    make \
    zlib1g-dev \
    libssl-dev \
    gperf \
    && rm -rf /var/lib/apt/lists/*

# Clone and build OpenSSL, static linking is preferred to reduce dependencies
ARG OPENSSL_VERSION=openssl-3.0.0
RUN git clone --depth 1 --branch ${OPENSSL_VERSION} https://github.com/openssl/openssl.git /openssl && \
    cd /openssl && \
    ./config no-shared --prefix=/usr/local/openssl && \
    make -j$(nproc) && \
    make install_sw

# Clone and build TDLib
RUN git clone --depth 1 https://github.com/tdlib/td.git /tdlib && \
    mkdir /tdlib/build && \
    cd /tdlib/build && \
    cmake -DCMAKE_BUILD_TYPE=Release -DOPENSSL_ROOT_DIR=/usr/local/openssl .. && \
    cmake --build . --target install

# Copy the local package files to the container's workspace
ADD . /app

# Set environment variables for CGO to find TDLib and OpenSSL
ENV CGO_CFLAGS="-I/usr/local/include -I/usr/local/openssl/include"
ENV CGO_LDFLAGS="-L/usr/local/lib -L/usr/local/openssl/lib -lssl -lcrypto -ltdjson_static -ltdcore -ltdactor -ltdapi -ltdutils"

# Build the Go app
RUN go mod download && \
    go build -o /webBridgeBot .

# Use a smaller image to run the app
FROM debian:buster-slim

# Copy the binary and libraries from the builder image
COPY --from=builder /webBridgeBot /webBridgeBot
COPY --from=builder /usr/local/lib /usr/local/lib
COPY --from=builder /usr/local/openssl/lib /usr/local/openssl/lib

# Update ld cache with the new shared libraries
RUN ldconfig

# Expose the application's port
EXPOSE 8080

# Run the webBridgeBot binary
CMD ["/webBridgeBot"]
