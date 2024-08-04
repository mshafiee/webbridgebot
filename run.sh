#!/bin/bash

# Default values for the environment variables
API_ID=${API_ID:-0}
API_HASH=${API_HASH:-""}
BOT_TOKEN=${BOT_TOKEN:-""}
BASE_URL=${BASE_URL:-"http://localhost:8080"}
PORT=${PORT:-"8080"}
HASH_LENGTH=${HASH_LENGTH:-8}
CACHE_DIRECTORY=${CACHE_DIRECTORY:-".cache"}
MAX_CACHE_SIZE=${MAX_CACHE_SIZE:-10737418240} # 10 GB in bytes
DEBUG_MODE=${DEBUG_MODE:-false}

# Execute the Go application with command-line flags
./webBridgeBot \
  -apiID="$API_ID" \
  -apiHash="$API_HASH" \
  -botToken="$BOT_TOKEN" \
  -baseURL="$BASE_URL" \
  -port="$PORT" \
  -hashLength="$HASH_LENGTH" \
  -cacheDirectory="$CACHE_DIRECTORY" \
  -maxCacheSize="$MAX_CACHE_SIZE" \
  -debugMode="$DEBUG_MODE"
