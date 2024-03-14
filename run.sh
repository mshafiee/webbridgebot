#!/bin/bash

# Default values for the environment variables
API_ID=${API_ID:-0}
API_HASH=${API_HASH:-""}
BOT_TOKEN=${BOT_TOKEN:-""}
BASE_URL=${BASE_URL:-"http://localhost:8080"}
PORT=${PORT:-"8080"}

# Execute the Go application with command-line flags
./webBridgeBot \
  -apiID="$API_ID" \
  -apiHash="$API_HASH" \
  -botToken="$BOT_TOKEN" \
  -baseURL="$BASE_URL" \
  -port="$PORT"
