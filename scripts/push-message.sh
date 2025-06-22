#!/bin/bash

# Default values from docker-compose.yml
PULSAR_URL="pulsar://localhost:6650"
TOPIC="public/transcribe/file-queue"
MESSAGE="demo.m4a"

# Override with command line argument if provided
if [ $# -gt 0 ]; then
    MESSAGE="$1"
fi

echo "Pushing message: $MESSAGE"
echo "To topic: $TOPIC"
echo "Pulsar URL: $PULSAR_URL"
echo ""

# Run the Go script with predefined values
go run cmd/push-message/main.go -url "$PULSAR_URL" -topic "$TOPIC" -message "$MESSAGE"