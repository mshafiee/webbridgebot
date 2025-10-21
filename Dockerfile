# Use the official Go image as a parent image
FROM golang:1.25.3 AS builder

# Set the working directory
WORKDIR /app

# Copy the local package files to the container's workspace
ADD . /app

# Build the Go app
RUN go mod download && \
    go build -o /app/webBridgeBot .

# Use a smaller image to run the app
FROM debian:bookworm-slim AS final

# Set the working directory
WORKDIR /app

# Copy the binary from the builder image
COPY --from=builder /app/webBridgeBot /app/webBridgeBot

# Copy the run script
COPY run.sh /app/run.sh

# Copy the run templates
COPY templates /app/templates

# Set the permissions for the binary and the run script
RUN chmod +x /app/webBridgeBot
RUN chmod +x /app/run.sh

# Expose the application's port
EXPOSE 8080

# Run the webBridgeBot binary
CMD ["/app/run.sh"]
