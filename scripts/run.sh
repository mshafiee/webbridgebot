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
LOG_CHANNEL_ID=${LOG_CHANNEL_ID:-"0"}

# Execute the Go application with command-line flags
./webBridgeBot \
  --api_id="$API_ID" \
  --api_hash="$API_HASH" \
  --bot_token="$BOT_TOKEN" \
  --base_url="$BASE_URL" \
  --port="$PORT" \
  --hash_length="$HASH_LENGTH" \
  --cache_directory="$CACHE_DIRECTORY" \
  --max_cache_size="$MAX_CACHE_SIZE" \
  --debug_mode="$DEBUG_MODE" \
  --log_channel_id="$LOG_CHANNEL_ID"
